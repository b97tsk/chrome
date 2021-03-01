package http

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"hash/crc32"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/b97tsk/chrome/internal/matchset"
	"github.com/b97tsk/chrome/internal/proxy"
	"github.com/b97tsk/chrome/service"
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
	File     service.EnvString
	Proxy    service.ProxyChain `yaml:"over"`
	hashCode uint32
}

func (r *RouteInfo) Equals(other *RouteInfo) bool {
	return r.File == other.File &&
		r.hashCode == other.hashCode &&
		r.Proxy.Equals(other.Proxy)
}

func (r *RouteInfo) Init(fsys fs.FS) error {
	hashCode, err := getHashCode(fsys, r.File.String())
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
}

type routeMatchSet struct {
	includes matchset.MatchSet
	excludes matchset.MatchSet
}

type patternConfig struct {
	ports []string
}

func (r *route) Recycle(r2 *route) {
	r.hash = r2.hash
	r.matchset.Store(r2.matchset.Load())
}

func (r *route) Init(fsys fs.FS) error {
	hashCode, err := getHashCode(fsys, r.File.String())
	if err != nil {
		return err
	}

	if r.hash == hashCode {
		return errNotModified
	}

	file, err := fsys.Open(r.File.String())
	if err != nil {
		return err
	}
	defer file.Close()

	var includes, excludes matchset.MatchSet

	configMap := make(map[string]*patternConfig)

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
		config := configMap[pattern]

		if portSuffix != "" {
			if config == nil {
				config = &patternConfig{}
				configMap[pattern] = config
			}

			config.ports = append(config.ports, portSuffix[1:])
		}

		if !exclude {
			includes.Add(pattern, config)
		} else {
			excludes.Add(pattern, config)
		}
	}

	r.hash = hashCode
	r.matchset.Store(&routeMatchSet{includes, excludes})

	return nil
}

func match(set *matchset.MatchSet, host, port string) bool {
	for _, c := range set.MatchAll(host) {
		config := c.(*patternConfig)
		if config == nil {
			return true
		}

		for _, p := range config.ports {
			if p == port {
				return true
			}
		}
	}

	return false
}

func (r *route) Match(hostport string) bool {
	set, ok := r.matchset.Load().(*routeMatchSet)
	host, port, _ := net.SplitHostPort(hostport)

	return ok && match(&set.includes, host, port) && !match(&set.excludes, host, port)
}

func (r *route) getDialer() proxy.Dialer {
	type dialer struct {
		proxy.Dialer
	}

	if x := r.dialer.Load(); x != nil {
		return x.(*dialer).Dialer
	}

	d := r.Proxy.NewDialer()
	r.dialer.Store(&dialer{d})

	return d
}

type Service struct{}

func (Service) Name() string {
	return "http"
}

func (Service) Options() interface{} {
	return new(Options)
}

func (Service) Run(ctx service.Context) {
	ln, err := net.Listen("tcp", ctx.ListenAddr)
	if err != nil {
		ctx.Logger.Error(err)
		return
	}

	ctx.Logger.Infof("listening on %v", ln.Addr())
	defer ctx.Logger.Infof("stopped listening on %v", ln.Addr())

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

	handler := NewHandler(ctx, optsOut)
	defer handler.CloseIdleConnections()

	var (
		server     *http.Server
		serverDown chan error
	)

	initialize := func() {
		if server != nil {
			return
		}

		server = &http.Server{
			Handler:      handler,
			TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)), // Disable HTTP/2.
		}
		serverDown = make(chan error, 1)

		go func() {
			serverDown <- server.Serve(ln)
			close(serverDown)
		}()
	}

	defer func() {
		if server != nil {
			_ = server.Shutdown(context.Background())

			<-serverDown
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-serverDown:
			return
		case opts := <-ctx.Opts:
			if new, ok := opts.(*Options); ok {
				old := <-optsOut
				new := *new
				new.dialer = old.dialer
				new.routes = old.routes
				new.matches = old.matches

				for i := range new.Routes {
					_ = new.Routes[i].Init(ctx.Manager)
				}

				if !new.Proxy.Equals(old.Proxy) {
					new.dialer = new.Proxy.NewDialer()
				}

				if !routesEquals(new.Routes, old.Routes) {
					new.routes = make([]*route, len(new.Routes))
					new.matches = &sync.Map{}

					for i, r := range new.Routes {
						new.routes[i] = &route{RouteInfo: r}

						didRecycle := false

						for _, r2 := range old.routes {
							if r.hashCode == r2.hashCode {
								new.routes[i].Recycle(r2)

								didRecycle = true

								break
							}
						}

						if !didRecycle {
							switch err := new.routes[i].Init(ctx.Manager); err {
							case nil:
								ctx.Logger.Infof("loaded %v", r.File)
							case errNotModified:
							default:
								ctx.Logger.Errorf("fatal: %v", err)
								return // Consider fatal here.
							}
						}
					}
				}

				if !redirectsEquals(new.Redirects, old.Redirects) {
					handler.setRedirects(new.Redirects)
				}

				optsIn <- new

				initialize()
			}
		}
	}
}

type Handler struct {
	ctx       service.Context
	tr        *http.Transport
	redirects atomic.Value
}

func NewHandler(ctx service.Context, opts <-chan Options) *Handler {
	h := &Handler{ctx: ctx}

	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		opts := <-opts
		if opts.routes != nil {
			if r, ok := opts.matches.Load(addr); ok {
				opts.dialer = r.(*route).getDialer()
			} else {
				for _, r := range opts.routes {
					if r.Match(addr) {
						h.ctx.Logger.Infof("%v matches %v", r.File, addr)
						opts.dialer = r.getDialer()
						opts.matches.Store(addr, r)

						break
					}
				}
			}
		}

		return h.ctx.Manager.Dial(ctx, opts.dialer, network, addr, opts.Dial.Timeout)
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
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("Proxy-Connection")

	if req.Method == http.MethodConnect {
		h.handleConnect(rw, req)
		return
	}

	if req.Method == http.MethodGet && strings.EqualFold(req.Header.Get("Connection"), "Upgrade") {
		h.handleUpgrade(rw, req)
		return
	}

	if h.handleRedirect(rw, req) {
		return
	}

	resp, err := h.tr.RoundTrip(req)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()

	header := rw.Header()
	for key, values := range resp.Header {
		header[key] = values
	}

	rw.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(rw, resp.Body)
}

func (h *Handler) hijack(rw http.ResponseWriter, handle func(net.Conn)) {
	if _, ok := rw.(http.Hijacker); !ok {
		http.Error(rw, "http.ResponseWriter does not implement http.Hijacker.", http.StatusInternalServerError)
		return
	}

	conn, _, err := rw.(http.Hijacker).Hijack()
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	go handle(conn)
}

func (h *Handler) handleConnect(rw http.ResponseWriter, req *http.Request) {
	h.hijack(rw, func(conn net.Conn) {
		defer conn.Close()

		local, ctx := service.NewConnChecker(conn)

		remoteHost := h.rewriteHost(req.RequestURI)

		remote, err := h.tr.DialContext(ctx, "tcp", remoteHost)
		if err != nil {
			h.ctx.Logger.Tracef("handleConnect: dial to remote: %v", err)
			return
		}
		defer remote.Close()

		httpVersion := "HTTP/1.0"
		if req.ProtoAtLeast(1, 1) {
			httpVersion = "HTTP/1.1"
		}

		responseString := httpVersion + " 200 OK\r\n\r\n"
		if _, err := local.Write([]byte(responseString)); err != nil {
			h.ctx.Logger.Tracef("handleConnect: write response to local: %v", err)
			return
		}

		service.Relay(local, remote)
	})
}

func (h *Handler) handleUpgrade(rw http.ResponseWriter, req *http.Request) {
	h.hijack(rw, func(conn net.Conn) {
		defer conn.Close()

		local, ctx := service.NewConnChecker(conn)

		remoteHost := h.rewriteHost(req.Host)

		remote, err := h.tr.DialContext(ctx, "tcp", remoteHost)
		if err != nil {
			h.ctx.Logger.Tracef("handleUpgrade: dial to remote: %v", err)
			return
		}
		defer remote.Close()

		if err := req.Write(remote); err != nil {
			h.ctx.Logger.Tracef("handleUpgrade: write request to remote: %v", err)
			return
		}

		resp, err := http.ReadResponse(bufio.NewReader(remote), req)
		if err != nil {
			h.ctx.Logger.Tracef("handleUpgrade: read response from remote: %v", err)
			return
		}
		defer resp.Body.Close()

		if err := resp.Write(local); err != nil {
			h.ctx.Logger.Tracef("handleUpgrade: write response to local: %v", err)
			return
		}

		service.Relay(local, remote)
	})
}

func (h *Handler) handleRedirect(rw http.ResponseWriter, req *http.Request) bool {
	redirects, _ := h.redirects.Load().(map[string]string)
	if s := redirects[req.URL.Host]; s != "" {
		if u, _ := url.Parse(s); u != nil {
			if u.Path == "" {
				u.Path = req.URL.Path
			}

			http.Redirect(rw, req, u.String(), http.StatusFound)

			return true
		}
	}

	return false
}

func (h *Handler) rewriteHost(host string) string {
	redirects, _ := h.redirects.Load().(map[string]string)
	if s := redirects[host]; s != "" {
		if u, _ := url.Parse(s); u != nil && u.Path == "" && u.Port() != "" {
			host = u.Host
		}
	}

	return host
}

func getHashCode(fsys fs.FS, name string) (hashCode uint32, err error) {
	file, err := fsys.Open(name)
	if err != nil {
		return
	}
	defer file.Close()

	digest := crc32.NewIEEE()

	_, err = io.Copy(digest, file)
	if err == nil {
		hashCode = digest.Sum32()
	}

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
