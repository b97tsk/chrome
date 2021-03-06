package http

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/b97tsk/chrome"
	"github.com/b97tsk/chrome/internal/httputil"
	"github.com/b97tsk/chrome/internal/matchset"
	"github.com/b97tsk/chrome/internal/netutil"
	"github.com/b97tsk/log"
	"golang.org/x/net/http/httpguts"
)

type Options struct {
	ListenAddr string `yaml:"on"`

	Routes    []RouteOptions
	Redirects map[string]string

	Proxy chrome.Proxy `yaml:"over"`

	Dial struct {
		Timeout time.Duration
	}
	Relay chrome.RelayOptions

	routes  []*route
	matches *sync.Map
}

type RouteOptions struct {
	File  chrome.EnvString
	Proxy chrome.Proxy `yaml:"over"`

	checksum []byte
}

func (r *RouteOptions) equal(other *RouteOptions) bool {
	return r.File == other.File && r.Proxy.Equal(other.Proxy) && bytes.Equal(r.checksum, other.checksum)
}

func (r *RouteOptions) init(fsys fs.FS) (err error) {
	r.checksum, err = checksum(fsys, r.File.String())
	return
}

type route struct {
	RouteOptions
	matchset atomic.Value
}

type routeMatchSet struct {
	includes matchset.MatchSet
	excludes matchset.MatchSet
}

type patternConfig struct {
	ports []string
}

func (r *route) Recycle(routes []*route) bool {
	if r.checksum == nil {
		return false
	}

	for _, r2 := range routes {
		if bytes.Equal(r.checksum, r2.checksum) {
			r.matchset.Store(r2.matchset.Load())
			return true
		}
	}

	return false
}

func (r *route) Init(fsys fs.FS) error {
	checksum1, err := checksum(fsys, r.File.String())
	if err != nil {
		return err
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

		portSuffix := rePortSuffix.FindString(line)
		pattern := line[:len(line)-len(portSuffix)]

		switch {
		case strings.HasPrefix(pattern, "[") && strings.HasSuffix(pattern, "]"):
			pattern = pattern[1 : len(pattern)-1]
		case portSuffix != "" && strings.Contains(pattern, ":"):
			// Assume the whole line is an IPv6 address.
			portSuffix = ""
			pattern = line
		}

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

	r.checksum = checksum1
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

type Service struct{}

const ServiceName = "http"

func (Service) Name() string {
	return ServiceName
}

func (Service) Options() interface{} {
	return new(Options)
}

func (Service) Run(ctx chrome.Context) {
	logger := ctx.Manager.Logger(ServiceName)

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

	handler := newHandler(ctx, optsOut)
	defer handler.CloseIdleConnections()

	var (
		server         *http.Server
		serverDown     chan struct{}
		serverListener net.Listener
	)

	startServer := func() error {
		if server != nil {
			return nil
		}

		ln, err := net.Listen("tcp", (<-optsOut).ListenAddr)
		if err != nil {
			logger.Error(err)
			return err
		}

		defer logger.Infof("listening on %v", ln.Addr())

		server = &http.Server{
			Handler:      handler,
			TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)), // Disable HTTP/2.
			ErrorLog:     logger.Get(log.LevelDebug),
		}
		serverDown = make(chan struct{})
		serverListener = ln

		go func() {
			_ = server.Serve(ln)

			close(serverDown)
		}()

		return nil
	}

	stopServer := func() {
		if server == nil {
			return
		}

		defer logger.Infof("stopped listening on %v", serverListener.Addr())

		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()

		_ = server.Shutdown(ctx)

		server = nil
		serverDown = nil
		serverListener = nil
	}
	defer stopServer()

	for {
		select {
		case <-ctx.Done():
			return
		case <-serverDown:
			return
		case opts := <-ctx.Load:
			if new, ok := opts.(*Options); ok {
				old := <-optsOut
				new := *new
				new.routes = old.routes
				new.matches = old.matches

				if _, _, err := net.SplitHostPort(new.ListenAddr); err != nil {
					logger.Error(err)
					return
				}

				if new.ListenAddr != old.ListenAddr {
					stopServer()
				}

				for i := range new.Routes {
					_ = new.Routes[i].init(ctx.Manager)
				}

				if !routesEqual(new.Routes, old.Routes) {
					new.routes = make([]*route, len(new.Routes))
					new.matches = &sync.Map{}

					for i, r := range new.Routes {
						new.routes[i] = &route{RouteOptions: r}
						if new.routes[i].Recycle(old.routes) {
							continue
						}

						if err := new.routes[i].Init(ctx.Manager); err != nil {
							logger.Error(err)
							return
						}

						logger.Infof("loaded %v", r.File)
					}
				}

				if !redirectsEqual(new.Redirects, old.Redirects) {
					handler.setRedirects(new.Redirects)
				}

				optsIn <- new
			}
		case <-ctx.Loaded:
			if err := startServer(); err != nil {
				return
			}
		}
	}
}

type handler struct {
	ctx       chrome.Context
	opts      <-chan Options
	tr        *http.Transport
	redirects atomic.Value
}

func newHandler(ctx chrome.Context, opts <-chan Options) *handler {
	h := &handler{ctx: ctx, opts: opts}

	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		opts := <-h.opts
		d := opts.Proxy.Dialer()

		if opts.routes != nil {
			if r, ok := opts.matches.Load(addr); ok {
				d = r.(*route).Proxy.Dialer()
			} else {
				for _, r := range opts.routes {
					if r.Match(addr) {
						h.ctx.Manager.Logger(ServiceName).Infof("%v matches %v", r.File, addr)

						d = r.Proxy.Dialer()
						opts.matches.Store(addr, r)

						break
					}
				}
			}
		}

		return h.ctx.Manager.Dial(ctx, d, network, addr, opts.Dial.Timeout)
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

func (h *handler) CloseIdleConnections() {
	h.tr.CloseIdleConnections()
}

func (h *handler) setRedirects(redirects map[string]string) {
	h.redirects.Store(redirects)
}

func (h *handler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodConnect {
		h.handleConnect(rw, req)
		return
	}

	outreq := req.Clone(req.Context())
	outreq.Close = false
	outreq.RequestURI = ""

	var upgradeType string
	if httpguts.HeaderValuesContainsToken(req.Header["Connection"], "upgrade") {
		upgradeType = req.Header.Get("Upgrade")
	}

	httputil.RemoveHopbyhopHeaders(outreq.Header)

	if _, ok := outreq.Header["User-Agent"]; !ok {
		outreq.Header.Set("User-Agent", "")
	}

	if upgradeType != "" {
		outreq.Header.Set("Connection", "upgrade")
		outreq.Header.Set("Upgrade", upgradeType)

		h.handleUpgrade(rw, outreq)

		return
	}

	if h.handleRedirect(rw, outreq) {
		return
	}

	resp, err := h.tr.RoundTrip(outreq)
	if err != nil {
		h.ctx.Manager.Logger(ServiceName).Trace(err)
		panic(http.ErrAbortHandler)
	}
	defer resp.Body.Close()

	httputil.RemoveHopbyhopHeaders(resp.Header)

	header := rw.Header()
	for key, values := range resp.Header {
		header[key] = values
	}

	if _, ok := header["Content-Length"]; !ok && resp.ContentLength >= 0 {
		header.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}

	rw.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(rw, resp.Body)
}

func (h *handler) hijack(rw http.ResponseWriter, handle func(net.Conn)) {
	if _, ok := rw.(http.Hijacker); !ok {
		h.ctx.Manager.Logger(ServiceName).Debug("hijack: impossible")
		panic(http.ErrAbortHandler)
	}

	conn, _, err := rw.(http.Hijacker).Hijack()
	if err != nil {
		h.ctx.Manager.Logger(ServiceName).Debugf("hijack: %v", err)
		panic(http.ErrAbortHandler)
	}

	go handle(conn)
}

func (h *handler) handleConnect(rw http.ResponseWriter, req *http.Request) {
	h.hijack(rw, func(conn net.Conn) {
		remoteHost := h.rewriteHost(req.RequestURI)

		getRemote := func(localCtx context.Context) net.Conn {
			remote, err := h.tr.DialContext(localCtx, "tcp", remoteHost)
			if err != nil {
				h.ctx.Manager.Logger(ServiceName).Tracef("connect: dial to remote: %v", err)
				return nil
			}

			return remote
		}

		sendResponse := func(w io.Writer) bool {
			const response = "HTTP/1.1 200 OK\r\n\r\n"
			if _, err := w.Write([]byte(response)); err != nil {
				h.ctx.Manager.Logger(ServiceName).Tracef("connect: write response to local: %v", err)
				return false
			}

			return true
		}

		h.ctx.Manager.Relay(conn, getRemote, sendResponse, (<-h.opts).Relay)
	})
}

func (h *handler) handleUpgrade(rw http.ResponseWriter, req *http.Request) {
	h.hijack(rw, func(conn net.Conn) {
		var b bytes.Buffer
		if err := req.Write(&b); err != nil {
			h.ctx.Manager.Logger(ServiceName).Tracef("upgrade: write request to buffer: %v", err)
			return
		}

		remoteHost := h.rewriteHost(req.Host)

		getRemote := func(localCtx context.Context) net.Conn {
			remote, err := h.tr.DialContext(localCtx, "tcp", remoteHost)
			if err != nil {
				h.ctx.Manager.Logger(ServiceName).Tracef("upgrade: dial to remote: %v", err)
				return nil
			}

			return remote
		}

		h.ctx.Manager.Relay(netutil.Unread(conn, b.Bytes()), getRemote, nil, (<-h.opts).Relay)
	})
}

func (h *handler) handleRedirect(rw http.ResponseWriter, req *http.Request) bool {
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

func (h *handler) rewriteHost(host string) string {
	redirects, _ := h.redirects.Load().(map[string]string)
	if s := redirects[host]; s != "" {
		if u, _ := url.Parse(s); u != nil && u.Path == "" && u.Port() != "" {
			host = u.Host
		}
	}

	return host
}

func checksum(fsys fs.FS, name string) ([]byte, error) {
	file, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	h := sha1.New()
	if _, err := io.Copy(h, file); err != nil {
		return nil, err
	}

	return h.Sum(make([]byte, 0, h.Size())), nil
}

func routesEqual(a, b []RouteOptions) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if !a[i].equal(&b[i]) {
			return false
		}
	}

	return true
}

func redirectsEqual(a, b map[string]string) bool {
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

var rePortSuffix = regexp.MustCompile(`:\d+$`)
