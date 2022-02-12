package http

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"path"
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

	routes       []route
	routeCache   *sync.Map
	allowListMap map[string]*allowList
}

type RouteOptions struct {
	AllowList chrome.EnvString `yaml:"file"`
	Proxy     chrome.Proxy     `yaml:"over"`

	AllowListChecksum string `yaml:"-"`
}

func (r *RouteOptions) equal(other *RouteOptions) bool {
	return r.AllowList == other.AllowList &&
		r.AllowListChecksum == other.AllowListChecksum &&
		r.Proxy.Equal(other.Proxy)
}

type route struct {
	AllowList *allowList
	Proxy     chrome.Proxy
}

type allowList struct {
	Path     string
	Checksum string

	includes matchset.MatchSet
	excludes matchset.MatchSet
}

type patternConfig struct {
	ports []string
}

func (l *allowList) Init(fsys fs.FS) error {
	return l.loadFile(fsys, l.Path)
}

func (l *allowList) loadFile(fsys fs.FS, name string) error {
	file, err := fsys.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()

	configMap := make(map[string]*patternConfig)

	s := bufio.NewScanner(file)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || line[0] == '#' {
			continue
		}

		if line[0] == '@' {
			filepath := line[1:]
			if strings.HasPrefix(filepath, "./") || strings.HasPrefix(filepath, "../") {
				filepath = path.Join(path.Dir(name), filepath)
			}

			if err := l.loadFile(fsys, filepath); err != nil {
				return fmt.Errorf("load %v: %w", name, err)
			}

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
			l.includes.Add(pattern, config)
		} else {
			l.excludes.Add(pattern, config)
		}
	}

	return s.Err()
}

func checksum(fsys fs.FS, name string) ([]byte, error) {
	h := sha1.New()
	if err := _checksum(fsys, name, h); err != nil {
		return nil, err
	}

	return h.Sum(make([]byte, 0, h.Size())), nil
}

func _checksum(fsys fs.FS, name string, digest io.Writer) error {
	file, err := fsys.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()

	sep := []byte{'\n'}

	s := bufio.NewScanner(file)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || line[0] == '#' {
			continue
		}

		if line[0] == '@' {
			filepath := line[1:]
			if strings.HasPrefix(filepath, "./") || strings.HasPrefix(filepath, "../") {
				filepath = path.Join(path.Dir(name), filepath)
			}

			if err := _checksum(fsys, filepath, digest); err != nil {
				return fmt.Errorf("load %v: %w", name, err)
			}

			continue
		}

		_, _ = digest.Write([]byte(line))
		_, _ = digest.Write(sep)
	}

	return s.Err()
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

func (l *allowList) Allow(hostport string) bool {
	host, port, _ := net.SplitHostPort(hostport)
	return match(&l.includes, host, port) && !match(&l.excludes, host, port)
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
				new.routeCache = old.routeCache
				new.allowListMap = old.allowListMap

				if _, _, err := net.SplitHostPort(new.ListenAddr); err != nil {
					logger.Error(err)
					return
				}

				if new.ListenAddr != old.ListenAddr {
					stopServer()
				}

				for i := range new.Routes {
					r := &new.Routes[i]
					checksum1, _ := checksum(ctx.Manager, r.AllowList.String())
					r.AllowListChecksum = hex.EncodeToString(checksum1)
				}

				if !routesEqual(new.Routes, old.Routes) {
					new.routes = nil
					new.routeCache = nil
					new.allowListMap = nil

					for i := range new.Routes {
						r := &new.Routes[i]

						l := old.allowListMap[r.AllowList.String()]
						if l == nil || l.Checksum != r.AllowListChecksum {
							l = &allowList{
								Path:     r.AllowList.String(),
								Checksum: r.AllowListChecksum,
							}

							if err := l.Init(ctx.Manager); err != nil {
								logger.Error(err)
								return
							}

							logger.Infof("loaded %v", r.AllowList)
						}

						if new.allowListMap == nil {
							new.allowListMap = make(map[string]*allowList)
						}

						new.routes = append(new.routes, route{l, r.Proxy})
						new.allowListMap[r.AllowList.String()] = l
					}

					if new.routes != nil {
						new.routeCache = &sync.Map{}
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

		if opts.routeCache != nil {
			if r, ok := opts.routeCache.Load(addr); ok {
				d = r.(*route).Proxy.Dialer()
			} else {
				for i := range opts.routes {
					if r := &opts.routes[i]; r.AllowList.Allow(addr) {
						h.ctx.Manager.Logger(ServiceName).Infof("%v matches %v", r.AllowList.Path, addr)

						d = r.Proxy.Dialer()
						opts.routeCache.Store(addr, r)

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
		if err != context.Canceled {
			h.ctx.Manager.Logger(ServiceName).Tracef("round trip: %v", err)
		}

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
		remoteAddr := h.rewriteHost(req.RequestURI)

		getRemote := func(localCtx context.Context) net.Conn {
			remote, err := h.tr.DialContext(localCtx, "tcp", remoteAddr)
			if err != nil && err != context.Canceled {
				h.ctx.Manager.Logger(ServiceName).Tracef("connect: dial %v: %v", remoteAddr, err)
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

		remoteAddr := h.rewriteHost(req.Host)

		getRemote := func(localCtx context.Context) net.Conn {
			remote, err := h.tr.DialContext(localCtx, "tcp", remoteAddr)
			if err != nil && err != context.Canceled {
				h.ctx.Manager.Logger(ServiceName).Tracef("upgrade: dial %v: %v", remoteAddr, err)
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
