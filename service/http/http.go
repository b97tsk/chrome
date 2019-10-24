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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/b97tsk/chrome/internal/matchset"
	"github.com/b97tsk/chrome/internal/proxy"
	"github.com/b97tsk/chrome/internal/utility"
	"github.com/b97tsk/chrome/service"
	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v2"
)

type Options struct {
	Routes []RouteInfo
	Proxy  service.ProxyChain `yaml:"over"`
}

type RouteInfo struct {
	File  string
	Proxy service.ProxyChain `yaml:"over"`
}

type route struct {
	RouteInfo
	hash     uint32
	dialer   proxy.Dialer
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
	file, err := os.Open(r.File)
	if err != nil {
		return err
	}
	defer file.Close()

	digest := crc32.NewIEEE()
	io.Copy(digest, file)
	file.Seek(0, io.SeekStart)
	hash := digest.Sum32()
	if hash == r.hash {
		return errNotModified
	}
	r.hash = hash

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
	for _, c := range matchset.Match(host) {
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

func (r *route) Dial(network, addr string) (net.Conn, error) {
	return r.DialContext(context.Background(), network, addr)
}

func (r *route) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if r.dialer == nil {
		r.dialer, _ = r.Proxy.NewDialer(service.Direct)
	}
	return proxy.Dial(ctx, r.dialer, network, addr)
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

	handler := NewHandler()
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
		options     Options
		watcher     *fsnotify.Watcher
		watchEvents <-chan fsnotify.Event
		watchErrors <-chan error
		delayTimers = make(map[string]*time.Timer)
		fileChanges = make(chan string)
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
				old := options
				options = new
				if !new.Proxy.Equals(old.Proxy) {
					d, _ := new.Proxy.NewDialer(service.Direct)
					handler.SetDial(
						func(ctx context.Context, network, addr string) (net.Conn, error) {
							return proxy.Dial(ctx, d, network, addr)
						},
					)
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
					newRoutes := make([]*route, len(new.Routes))
					oldRoutes := handler.getRoutes()
					if watcher != nil {
						for _, r := range oldRoutes {
							watcher.Remove(r.File)
						}
					}
					for i, r := range new.Routes {
						r.File = filepath.Clean(os.ExpandEnv(r.File))
						newRoutes[i] = &route{RouteInfo: r}
						if watcher != nil {
							err := watcher.Add(r.File)
							if err != nil {
								log.Printf("[http] watcher: %v\n", err)
							}
						}
						didRecycle := false
						for _, r2 := range oldRoutes {
							if r.File == r2.File {
								newRoutes[i].Recycle(r2)
								didRecycle = true
								break
							}
						}
						if !didRecycle {
							switch err := newRoutes[i].Init(); err {
							case nil:
								log.Printf("[http] loaded %v\n", r.File)
							case errNotModified:
							default:
								log.Printf("[http] fatal: %v\n", err)
								return // Consider fatal here.
							}
						}
					}
					handler.setRoutes(newRoutes)
				}
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
			routesChanged := false
			for _, r := range handler.getRoutes() {
				if r.File == name {
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
				timer := delayTimers["<reset-matches>"]
				if timer == nil {
					timer = time.AfterFunc(time.Second, func() {
						handler.clearMatches()
					})
					delayTimers["<reset-matches>"] = timer
				} else {
					timer.Reset(time.Second)
				}
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
	tr      *http.Transport
	dial    atomic.Value
	routes  atomic.Value
	matches atomic.Value
}

func NewHandler() *Handler {
	h := &Handler{}
	h.dial.Store(
		func(ctx context.Context, network, addr string) (net.Conn, error) {
			return proxy.Dial(ctx, service.Direct, network, addr)
		},
	)

	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dial := h.dial.Load().(func(context.Context, string, string) (net.Conn, error))
		if routes, _ := h.routes.Load().([]*route); routes != nil {
			matches := h.matches.Load().(*sync.Map)
			if r, ok := matches.Load(addr); ok {
				dial = r.(*route).DialContext
			} else {
				for _, r := range routes {
					if r.Match(addr) {
						log.Printf("[http] %v matches %v\n", r.File, addr)
						dial = r.DialContext
						matches.Store(addr, r)
						break
					}
				}
			}
		}
		return dial(ctx, network, addr)
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

func (h *Handler) SetDial(dial func(context.Context, string, string) (net.Conn, error)) {
	h.dial.Store(dial)
}

func (h *Handler) getRoutes() []*route {
	routes, _ := h.routes.Load().([]*route)
	return routes
}

func (h *Handler) setRoutes(routes []*route) {
	h.routes.Store(routes)
	h.matches.Store(&sync.Map{})
}

func (h *Handler) clearMatches() {
	h.matches.Store(&sync.Map{})
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

			utility.Relay(remote, conn)
		}()
		return
	}

	req.Header.Del("Connection")
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Proxy-Authenticate")
	req.Header.Del("Proxy-Authorization")

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

func routesEquals(a, b []RouteInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		r1, r2 := &a[i], &b[i]
		if r1.File != r2.File || !r1.Proxy.Equals(r2.Proxy) {
			return false
		}
	}
	return true
}

var (
	errNotModified = errors.New("not modified")

	regxPortSuffix = regexp.MustCompile(`:\d+$`)
)
