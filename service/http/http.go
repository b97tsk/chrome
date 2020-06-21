package http

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"hash/crc32"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/b97tsk/chrome/internal/matchset"
	"github.com/b97tsk/chrome/internal/proxy"
	"github.com/b97tsk/chrome/service"
	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v2"
)

type Options struct {
	Routes    []RouteInfo
	Redirects map[string]string
	Proxy     service.ProxyChain `yaml:"over"`
	Dial      struct {
		Timeout time.Duration
	}

	dialer  proxy.Dialer
	routes  []*route
	matches *sync.Map
}

type RouteInfo struct {
	File     service.String
	Proxy    service.ProxyChain `yaml:"over"`
	hashCode uint32
}

func (r *RouteInfo) Equals(other *RouteInfo) bool {
	return r.File == other.File &&
		r.hashCode == other.hashCode &&
		r.Proxy.Equals(other.Proxy)
}

func (r *RouteInfo) Init() error {
	hashCode, err := getHashCode(r.File.String())
	if err == nil {
		r.hashCode = hashCode
	}
	return err
}

type route struct {
	RouteInfo
	hash     uint32
	dialer   atomic.Value
	matchset atomic.Value
	excludes atomic.Value
}

type patternConfig struct {
	matchAllPorts   bool
	matchThosePorts []string
}

func (r *route) Recycle(r2 *route) {
	r.hash = r2.hash
	r.matchset.Store(r2.matchset.Load())
	r.excludes.Store(r2.excludes.Load())
}

func (r *route) Init() error {
	hashCode, err := getHashCode(r.File.String())
	if err != nil {
		return err
	}
	if r.hash == hashCode {
		return errNotModified
	}

	file, err := os.Open(r.File.String())
	if err != nil {
		return err
	}
	defer file.Close()

	var set, excludes matchset.MatchSet
	patternConfigs := make(map[string]*patternConfig)

	s := bufio.NewScanner(file)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		exclude := line[0] == '!'
		if exclude {
			line = line[1:]
		}
		portSuffix := regxPortSuffix.FindString(line)
		pattern := line[:len(line)-len(portSuffix)]
		config := patternConfigs[pattern]
		if config == nil {
			config = &patternConfig{}
			patternConfigs[pattern] = config
		}
		if portSuffix == "" {
			config.matchAllPorts = true
			config.matchThosePorts = nil
		} else if !config.matchAllPorts {
			config.matchThosePorts = append(config.matchThosePorts, portSuffix[1:])
		}
		if !exclude {
			set.Add(pattern, config)
		} else {
			excludes.Add(pattern, config)
		}
	}
	r.hash = hashCode
	r.matchset.Store(&set)
	r.excludes.Store(&excludes)
	return nil
}

func (r *route) Match(hostport string) bool {
	if !r.match(&r.matchset, hostport) {
		return false
	}
	return !r.match(&r.excludes, hostport)
}

func (r *route) match(set *atomic.Value, hostport string) bool {
	matchset, _ := set.Load().(*matchset.MatchSet)
	if matchset == nil {
		return false
	}
	host, port, _ := net.SplitHostPort(hostport)
	for _, c := range matchset.MatchAll(host) {
		config := c.(*patternConfig)
		if config.matchAllPorts {
			return true
		}
		for _, p := range config.matchThosePorts {
			if p == port {
				return true
			}
		}
	}
	return false
}

func (r *route) getDialer() proxy.Dialer {
	type dialer struct {
		proxy.Dialer
	}
	if x := r.dialer.Load(); x != nil {
		return x.(*dialer).Dialer
	}
	d, _ := r.Proxy.NewDialer(proxy.Direct)
	r.dialer.Store(&dialer{d})
	return d
}

type Service struct{}

func (Service) Name() string {
	return "http"
}

func (Service) Run(ctx service.Context) {
	ln, err := net.Listen("tcp", ctx.ListenAddr)
	if err != nil {
		log.Printf("[http] %v\n", err)
		return
	}
	log.Printf("[http] listening on %v\n", ln.Addr())
	defer log.Printf("[http] stopped listening on %v\n", ln.Addr())

	optsIn, optsOut := make(chan Options), make(chan Options)
	defer close(optsIn)
	go func() {
		var opts Options
		ok := true
		for ok {
			select {
			case opts, ok = <-optsIn:
			case optsOut <- opts:
			}
		}
		close(optsOut)
	}()

	handler := NewHandler(ctx.Manager, optsOut)
	server := http.Server{
		Handler:      handler,
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)), // Disable HTTP/2.
	}
	defer handler.CloseIdleConnections()

	serverDown := make(chan error, 1)
	defer func() {
		server.Shutdown(context.TODO())
		<-serverDown
	}()

	go func() {
		serverDown <- server.Serve(ln)
		close(serverDown)
	}()

	var (
		watcher      *fsnotify.Watcher
		watchEvents  <-chan fsnotify.Event
		watchErrors  <-chan error
		delayTimers  = make(map[string]*time.Timer)
		fileChanges  = make(chan string)
		resetMatches <-chan time.Time
	)
	defer func() {
		if watcher != nil {
			watcher.Close()
		}
	}()

	for {
		select {
		case data := <-ctx.Events:
			if new, ok := data.(Options); ok {
				old := <-optsOut
				new.dialer = old.dialer
				new.routes = old.routes
				new.matches = old.matches
				for i := range new.Routes {
					new.Routes[i].Init()
				}
				if !new.Proxy.Equals(old.Proxy) {
					new.dialer, _ = new.Proxy.NewDialer(proxy.Direct)
				}
				if !routesEquals(new.Routes, old.Routes) {
					if watcher == nil {
						var err error
						watcher, err = fsnotify.NewWatcher()
						if watcher != nil {
							watchEvents = watcher.Events
							watchErrors = watcher.Errors
						}
						if err != nil {
							log.Printf("[http] %v\n", err)
						}
					}
					new.routes = make([]*route, len(new.Routes))
					new.matches = &sync.Map{}
					if watcher != nil {
						for _, r := range old.routes {
							watcher.Remove(r.File.String())
						}
					}
					for i, r := range new.Routes {
						r.File = service.String(filepath.Clean(r.File.String()))
						new.routes[i] = &route{RouteInfo: r}
						if watcher != nil {
							err := watcher.Add(r.File.String())
							if err != nil {
								log.Printf("[http] watcher: %v\n", err)
							}
						}
						didRecycle := false
						for _, r2 := range old.routes {
							if r.File == r2.File && r.hashCode == r2.hashCode {
								new.routes[i].Recycle(r2)
								didRecycle = true
								break
							}
						}
						if !didRecycle {
							switch err := new.routes[i].Init(); err {
							case nil:
								log.Printf("[http] loaded %v\n", r.File)
							case errNotModified:
							default:
								log.Printf("[http] fatal: %v\n", err)
								return // Consider fatal here.
							}
						}
					}
				}
				if !redirectsEquals(new.Redirects, old.Redirects) {
					handler.setRedirects(new.Redirects)
				}
				optsIn <- new
			}
		case err := <-serverDown:
			log.Printf("[http] %v\n", err)
			return
		case <-ctx.Done:
			return
		case e := <-watchEvents:
			if e.Op&fsnotify.Write != 0 {
				timer := delayTimers[e.Name]
				if timer == nil {
					timer = time.AfterFunc(time.Second, func() {
						select {
						case fileChanges <- e.Name:
						case <-ctx.Done:
						}
					})
					delayTimers[e.Name] = timer
				} else {
					timer.Reset(time.Second)
				}
			}
		case err := <-watchErrors:
			log.Printf("[http] watcher: %v\n", err)
		case name := <-fileChanges:
			opts := <-optsOut
			routesChanged := false
			for _, r := range opts.routes {
				if r.File.String() == name {
					switch err := r.Init(); err {
					case nil:
						log.Printf("[http] loaded %v\n", r.File)
						routesChanged = true
					case errNotModified:
					default:
						log.Printf("[http] reload: %v\n", err)
					}
				}
			}
			delete(delayTimers, name)
			if routesChanged {
				resetMatches = time.After(time.Second)
			}
		case <-resetMatches:
			opts := <-optsOut
			if opts.routes != nil {
				opts.matches = &sync.Map{}
				optsIn <- opts
			}
		}
	}
}

func (Service) UnmarshalOptions(text []byte) (interface{}, error) {
	var options Options
	if err := yaml.UnmarshalStrict(text, &options); err != nil {
		return nil, err
	}
	return options, nil
}

type Handler struct {
	tr        *http.Transport
	redirects atomic.Value
}

func NewHandler(man *service.Manager, opts <-chan Options) *Handler {
	h := &Handler{}

	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		opts := <-opts
		if opts.routes != nil {
			if r, ok := opts.matches.Load(addr); ok {
				opts.dialer = r.(*route).getDialer()
			} else {
				for _, r := range opts.routes {
					if r.Match(addr) {
						log.Printf("[http] %v matches %v\n", r.File, addr)
						opts.dialer = r.getDialer()
						opts.matches.Store(addr, r)
						break
					}
				}
			}
		}
		return man.Dial(ctx, opts.dialer, network, addr, opts.Dial.Timeout)
	}

	h.tr = &http.Transport{
		DialContext:           dial,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       10 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return h
}

func (h *Handler) CloseIdleConnections() {
	h.tr.CloseIdleConnections()
}

func (h *Handler) setRedirects(redirects map[string]string) {
	h.redirects.Store(redirects)
}

func (h *Handler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodConnect {
		if _, ok := rw.(http.Hijacker); !ok {
			http.Error(rw, "http.ResponseWriter does not implement http.Hijacker.", http.StatusInternalServerError)
			return
		}
		conn, _, err := rw.(http.Hijacker).Hijack()
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		requestURI := req.RequestURI
		httpVersion := "HTTP/1.0"
		if req.ProtoAtLeast(1, 1) {
			httpVersion = "HTTP/1.1"
		}
		go func() {
			defer conn.Close()

			ctx, conn := service.CheckConnectivity(context.Background(), conn)

			remote, err := h.tr.DialContext(ctx, "tcp", requestURI)
			if err != nil {
				responseString := httpVersion + " 503 Service Unavailable\r\n\r\n"
				if _, err := conn.Write([]byte(responseString)); err != nil {
					log.Printf("[http] write: %v\n", err)
				}
				return
			}
			defer remote.Close()

			responseString := httpVersion + " 200 OK\r\n\r\n"
			if _, err := conn.Write([]byte(responseString)); err != nil {
				log.Printf("[http] write: %v\n", err)
				return
			}

			service.Relay(remote, conn)
		}()
		return
	}

	req.Header.Del("Connection")
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Proxy-Authenticate")
	req.Header.Del("Proxy-Authorization")

	redirects, _ := h.redirects.Load().(map[string]string)
	if s := redirects[req.URL.Host]; s != "" {
		u, err := url.Parse(s)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		if u.Path == "" {
			u.Path = req.URL.Path
		}
		http.Redirect(rw, req, u.String(), http.StatusFound)
		return
	}

	resp, err := h.tr.RoundTrip(req)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusServiceUnavailable)
		return
	}

	copyHeader(rw.Header(), resp.Header)
	rw.WriteHeader(resp.StatusCode)
	io.Copy(rw, resp.Body)
	resp.Body.Close()
}

func copyHeader(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func getHashCode(name string) (hashCode uint32, err error) {
	file, err := os.Open(name)
	if err != nil {
		return
	}
	digest := crc32.NewIEEE()
	_, err = io.Copy(digest, file)
	if err == nil {
		hashCode = digest.Sum32()
	}
	file.Close()
	return
}

func routesEquals(a, b []RouteInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equals(&b[i]) {
			return false
		}
	}
	return true
}

func redirectsEquals(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

var (
	errNotModified = errors.New("not modified")

	regxPortSuffix = regexp.MustCompile(`:\d+$`)
)
