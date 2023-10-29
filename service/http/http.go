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
	"net/netip"
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
	"github.com/b97tsk/chrome/internal/netiputil"
	"github.com/b97tsk/chrome/internal/netutil"
	"github.com/b97tsk/log"
	"golang.org/x/net/http/httpguts"
)

type Options struct {
	ListenAddr string `yaml:"on"`

	Proxy chrome.Proxy `yaml:"over"`

	Routes       []RouteOptions
	Redirects    map[string]string
	RewriteHosts map[string]string

	Dial  chrome.DialOptions
	Relay chrome.RelayOptions

	routes       []route
	routeCache   *sync.Map
	allowListMap map[string]*allowList
}

type RouteOptions struct {
	Name     string
	What     string
	Checksum string       `yaml:"-"`
	Proxy    chrome.Proxy `yaml:"over"`
}

func (r *RouteOptions) equal(other *RouteOptions) bool {
	return r.Name == other.Name && r.Checksum == other.Checksum && r.Proxy.Equal(other.Proxy)
}

type route struct {
	Name      string
	AllowList *allowList
	Proxy     chrome.Proxy
}

type allowList struct {
	What     string
	Checksum string

	includes, excludes struct {
		patterns matchset.MatchSet
		ipranges netiputil.AddrRangeSet
	}
}

type patternConfig struct {
	ports []string
}

func (l *allowList) Init(fsys fs.FS, logger *log.Logger) error {
	p := &allowListParser{logger: logger}

	s := bufio.NewScanner(strings.NewReader(l.What))
	s.Split(scanLines)

	for s.Scan() {
		if err := p.parseLine(fsys, ".", s.Text()); err != nil {
			return err
		}
	}

	for pattern, config := range p.includes.patterns {
		l.includes.patterns.Add(pattern, config)
	}

	for pattern, config := range p.excludes.patterns {
		l.excludes.patterns.Add(pattern, config)
	}

	l.includes.ipranges = p.includes.ipranges
	l.excludes.ipranges = p.excludes.ipranges

	return nil
}

type allowListParser struct {
	logger *log.Logger

	includes, excludes struct {
		patterns map[string]*patternConfig
		ipranges netiputil.AddrRangeSet
	}
}

func (p *allowListParser) parseLine(fsys fs.FS, name, line string) error {
	if line[0] == '@' {
		filepath := line[1:]
		if strings.HasPrefix(filepath, "./") || strings.HasPrefix(filepath, "../") {
			filepath = path.Join(path.Dir(name), filepath)
		}

		if err := p.parseFile(fsys, filepath); err != nil {
			return fmt.Errorf("load %v: %w", name, err)
		}

		return nil
	}

	exclude := line[0] == '!'
	if exclude {
		line = line[1:]
	}

	settings := &p.includes
	if exclude {
		settings = &p.excludes
	}

	if strings.Contains(line, "/") {
		r, closed, err := netiputil.ParsePrefix(line)
		if err != nil {
			return fmt.Errorf("load %v: %w", name, err)
		}

		settings.ipranges.Add(r)

		if !closed {
			return nil
		}

		line = r.High.String() // Either 255.255.255.255 or ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff.
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

	if settings.patterns == nil {
		settings.patterns = make(map[string]*patternConfig)
	}

	if portSuffix == "" {
		settings.patterns[pattern] = nil
		return nil
	}

	config, configExists := settings.patterns[pattern]
	if config == nil {
		if configExists {
			return nil
		}

		config = &patternConfig{}
		settings.patterns[pattern] = config
	}

	port := portSuffix[1:]

	for _, p := range config.ports {
		if p == port {
			return nil
		}
	}

	config.ports = append(config.ports, port)

	return nil
}

func (p *allowListParser) parseFile(fsys fs.FS, name string) error {
	file, err := fsys.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()

	s := bufio.NewScanner(file)
	s.Split(scanLines)

	for s.Scan() {
		if err := p.parseLine(fsys, name, s.Text()); err != nil {
			return err
		}
	}

	if err := s.Err(); err != nil {
		return err
	}

	if p.logger != nil {
		p.logger.Infof("loaded %v", name)
	}

	return nil
}

func checksum(fsys fs.FS, what string) ([]byte, error) {
	h := sha1.New()

	s := bufio.NewScanner(strings.NewReader(what))
	s.Split(scanLines)

	for s.Scan() {
		if err := checksumLine(fsys, ".", s.Text(), h); err != nil {
			return nil, err
		}
	}

	return h.Sum(make([]byte, 0, h.Size())), nil
}

var sep = []byte("\n")

func checksumLine(fsys fs.FS, name, line string, digest io.Writer) error {
	if line[0] == '@' {
		filepath := line[1:]
		if strings.HasPrefix(filepath, "./") || strings.HasPrefix(filepath, "../") {
			filepath = path.Join(path.Dir(name), filepath)
		}

		if err := checksumFile(fsys, filepath, digest); err != nil {
			return fmt.Errorf("load %v: %w", name, err)
		}

		return nil
	}

	_, _ = digest.Write([]byte(line))
	_, _ = digest.Write(sep)

	return nil
}

func checksumFile(fsys fs.FS, name string, digest io.Writer) error {
	file, err := fsys.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()

	s := bufio.NewScanner(file)
	s.Split(scanLines)

	for s.Scan() {
		if err := checksumLine(fsys, name, s.Text(), digest); err != nil {
			return err
		}
	}

	return s.Err()
}

func (l *allowList) Allow(host, port string, addr netip.Addr) bool {
	return match(&l.includes.patterns, l.includes.ipranges, host, port, addr) &&
		!match(&l.excludes.patterns, l.excludes.ipranges, host, port, addr)
}

func match(patterns *matchset.MatchSet, ipranges netiputil.AddrRangeSet, host, port string, addr netip.Addr) bool {
	if addr.IsValid() && ipranges.Contains(addr) {
		return true
	}

	for _, v := range patterns.MatchAll(host) {
		config := v.(*patternConfig)
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

type Service struct{}

const ServiceName = "http"

func (Service) Name() string {
	return ServiceName
}

func (Service) Options() any {
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

		go func(srv *http.Server, down chan<- struct{}) {
			_ = srv.Serve(ln)

			close(down)
		}(server, serverDown)

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
		case ev := <-ctx.Event:
			switch ev := ev.(type) {
			case chrome.LoadEvent:
				old := <-optsOut
				new := *ev.Options.(*Options)
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

					if r.Name == "" {
						if advance, token, _ := scanLines([]byte(r.What), true); advance == len(r.What) {
							r.Name = string(token)
						}
					}

					if r.Name == "" {
						r.Name = fmt.Sprintf("#%v", i+1)
					}

					sum, err := checksum(ctx.Manager, r.What)
					if err != nil {
						logger.Error(err)
						return
					}

					r.Checksum = hex.EncodeToString(sum)
				}

				if !routesEqual(new.Routes, old.Routes) {
					new.routes = nil
					new.routeCache = nil
					new.allowListMap = nil

					for i := range new.Routes {
						r := &new.Routes[i]

						l := old.allowListMap[r.Name]
						if l == nil || l.Checksum != r.Checksum {
							l = &allowList{
								What:     r.What,
								Checksum: r.Checksum,
							}

							if err := l.Init(ctx.Manager, logger); err != nil {
								logger.Error(err)
								return
							}
						}

						if new.allowListMap == nil {
							new.allowListMap = make(map[string]*allowList)
						}

						new.routes = append(new.routes, route{r.Name, l, r.Proxy})
						new.allowListMap[r.Name] = l
					}

					if new.routes != nil {
						new.routeCache = &sync.Map{}
					}
				}

				if !mapEqual(new.Redirects, old.Redirects) {
					handler.setRedirects(new.Redirects)
				}

				if !mapEqual(new.RewriteHosts, old.RewriteHosts) {
					handler.setRewriteHosts(new.RewriteHosts)
				}

				optsIn <- new
			case chrome.LoadedEvent:
				if err := startServer(); err != nil {
					return
				}
			}
		}
	}
}

type handler struct {
	ctx       chrome.Context
	opts      <-chan Options
	tr        *http.Transport
	redirects atomic.Value
	rwhosts   atomic.Value
}

func newHandler(ctx chrome.Context, opts <-chan Options) *handler {
	h := &handler{ctx: ctx, opts: opts}

	logger := ctx.Manager.Logger(ServiceName)

	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		getopts := func() (chrome.Proxy, chrome.DialOptions, bool) {
			opts, ok := <-h.opts

			if opts.routeCache != nil {
				if r, ok := opts.routeCache.Load(addr); ok {
					opts.Proxy = r.(*route).Proxy
				} else {
					host, port, _ := net.SplitHostPort(addr)

					addr1, _ := netip.ParseAddr(host)
					if addr1.IsValid() {
						addr1 = addr1.Unmap()
						host = addr1.String()
					}

					for i := range opts.routes {
						if r := &opts.routes[i]; r.AllowList.Allow(host, port, addr1) {
							logger.Infof("%v matches %v", r.Name, addr)

							opts.Proxy = r.Proxy
							opts.routeCache.Store(addr, r)

							break
						}
					}
				}
			}

			return opts.Proxy, opts.Dial, ok
		}

		return h.ctx.Manager.Dial(ctx, network, addr, getopts, logger)
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

func (h *handler) setRedirects(m map[string]string) {
	h.redirects.Store(m)
}

func (h *handler) setRewriteHosts(m map[string]string) {
	h.rwhosts.Store(m)
}

func (h *handler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodConnect {
		h.handleConnect(rw, req)
		return
	}

	var upgradeType string
	if httpguts.HeaderValuesContainsToken(req.Header["Connection"], "Upgrade") {
		upgradeType = req.Header.Get("Upgrade")
	}

	outreq := req.Clone(req.Context())
	outreq.Close = false
	outreq.RequestURI = ""

	httputil.RemoveHopbyhopHeaders(outreq.Header)

	if _, ok := outreq.Header["User-Agent"]; !ok {
		outreq.Header.Set("User-Agent", "")
	}

	if upgradeType != "" {
		outreq.Header.Set("Connection", "Upgrade")
		outreq.Header.Set("Upgrade", upgradeType)

		h.handleUpgrade(rw, outreq)

		return
	}

	if h.handleRedirect(rw, outreq) {
		return
	}

	resp, err := h.tr.RoundTrip(outreq)
	if err != nil {
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

		getopts := func() (chrome.RelayOptions, bool) {
			opts, ok := <-h.opts
			return opts.Relay, ok
		}

		getRemote := func(localCtx context.Context) net.Conn {
			remote, _ := h.tr.DialContext(localCtx, "tcp", remoteAddr)
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

		logger := h.ctx.Manager.Logger(ServiceName)

		h.ctx.Manager.Relay(conn, getopts, getRemote, sendResponse, logger)
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

		getopts := func() (chrome.RelayOptions, bool) {
			opts, ok := <-h.opts
			return opts.Relay, ok
		}

		getRemote := func(localCtx context.Context) net.Conn {
			remote, _ := h.tr.DialContext(localCtx, "tcp", remoteAddr)
			return remote
		}

		logger := h.ctx.Manager.Logger(ServiceName)

		h.ctx.Manager.Relay(netutil.Unread(conn, b.Bytes()), getopts, getRemote, nil, logger)
	})
}

func (h *handler) handleRedirect(rw http.ResponseWriter, req *http.Request) bool {
	redirects, _ := h.redirects.Load().(map[string]string)
	if s := redirects[req.Host]; s != "" {
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
	rwhosts, _ := h.rwhosts.Load().(map[string]string)
	if s := rwhosts[host]; s != "" && strings.Contains(s, ":") {
		return s
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

func mapEqual(a, b map[string]string) bool {
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

func scanLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	start := 0

Again:
	advance, token, err = bufio.ScanLines(data[start:], atEOF)
	if err != nil {
		return 0, nil, err
	}

	start += advance

	if token != nil {
		token = bytes.TrimSpace(token)

		if len(token) == 0 || bytes.HasPrefix(token, []byte("#")) {
			goto Again
		}
	}

	return start, token, nil
}

var rePortSuffix = regexp.MustCompile(`:\d+$`)
