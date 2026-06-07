package dnstun

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"path"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"codeberg.org/miekg/dns"
	"codeberg.org/miekg/dns/dnshttp"
	"codeberg.org/miekg/dns/dnsutil"
	"github.com/b97tsk/chrome"
	"github.com/b97tsk/chrome/internal/matchset"
	"github.com/b97tsk/chrome/internal/netutil"
	"github.com/b97tsk/chrome/proxy"
)

type Options struct {
	ListenAddr string `yaml:"on"`

	Proxy chrome.Proxy `yaml:"over"`

	Server  DNServer
	Servers []DNServer

	Routes    []RouteOptions
	Redirects map[string]string

	Cache bool

	IPv4 struct {
		Only bool
	}

	Conn  chrome.ConnOptions
	Relay chrome.RelayOptions

	TTL struct {
		Min, Max time.Duration
	}
	Idle struct {
		Timeout time.Duration
	}
	Read struct {
		Timeout time.Duration
	}
	Write struct {
		Timeout time.Duration
	}

	routes       []route
	routeCache   *sync.Map
	allowListMap map[string]*allowList

	dnsCache *sync.Map
}

type DNServer struct {
	Name   string
	IP     chrome.StringList
	Over   string
	Port   uint16
	Path   string
	Method string
}

func (s *DNServer) method() string {
	if s.Method == "GET" {
		return "GET"
	}
	return "POST"
}

func defaultPort(protocol string) int {
	switch protocol {
	case "TLS":
		return 853
	case "HTTPS":
		return 443
	case "HTTP":
		return 80
	default:
		return 53
	}
}

func (s *DNServer) port() int {
	if s.Port != 0 {
		return int(s.Port)
	}
	return defaultPort(s.Over)
}

func (s *DNServer) url() string {
	var b strings.Builder
	b.WriteString(strings.ToLower(cmp.Or(s.Over, "TCP")))
	b.WriteString("://")
	b.WriteString(s.Name)
	if s.Port != 0 && int(s.Port) != defaultPort(s.Over) {
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(int(s.Port)))
	}
	return b.String()
}

type RouteOptions struct {
	Name     string
	What     string
	Checksum string       `yaml:"-"`
	Proxy    chrome.Proxy `yaml:"over"`
	Servers  []DNServer
}

func (r *RouteOptions) equal(other *RouteOptions) bool {
	return r.Name == other.Name && r.Checksum == other.Checksum &&
		r.Proxy.Equal(other.Proxy) && reflect.DeepEqual(r.Servers, other.Servers)
}

type route struct {
	Name      string
	AllowList *allowList
	Proxy     chrome.Proxy
	Servers   []DNServer
}

type allowList struct {
	What     string
	Checksum string

	includes, excludes struct {
		patterns matchset.MatchSet
	}
}

func (l *allowList) Init(fsys fs.FS, logger *slog.Logger) error {
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

	return nil
}

type allowListParser struct {
	logger *slog.Logger

	includes, excludes struct {
		patterns map[string]struct{}
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
		return nil // Ignore CIDRs.
	}

	portSuffix := rePortSuffix.FindString(line)
	pattern := line[:len(line)-len(portSuffix)]

	switch {
	case strings.HasPrefix(pattern, "[") && strings.HasSuffix(pattern, "]"):
		pattern = pattern[1 : len(pattern)-1]
	case portSuffix != "" && strings.Contains(pattern, ":"):
		// Assume the whole line is an IPv6 address.
		return nil // Ignore IPv6 addresses.
	}

	if settings.patterns == nil {
		settings.patterns = make(map[string]struct{})
	}

	settings.patterns[pattern] = struct{}{}

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
		p.logger.Info("parsefile", slog.String("path", name))
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

func (l *allowList) Allow(domain string) bool {
	return l.includes.patterns.MatchAll(domain) != nil && l.excludes.patterns.MatchAll(domain) == nil
}

type dnsCacheKey struct {
	Fqdn  string
	Qtype uint16
}

type Service struct{}

const ServiceName = "dnstun"

func (Service) Name() string {
	return ServiceName
}

func (Service) Options() any {
	return new(Options)
}

func (Service) Run(ctx chrome.Context) {
	logger := ctx.Manager.Logger().With(slog.String("job", ctx.JobName))

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

	var dnsQueryIn chan dnsQuery

	var server net.Listener

	startServer := func() error {
		if server != nil {
			return nil
		}

		ln, err := net.Listen("tcp", (<-optsOut).ListenAddr)
		if err != nil {
			logger.Error("net:listen", slog.Any("error", err))
			return err
		}

		defer logger.Info("net:listening", slog.Any("addr", ln.Addr()))

		server = ln

		if dnsQueryIn == nil {
			dnsQueryIn = make(chan dnsQuery)

			go startWorker(ctx, logger, optsOut, dnsQueryIn)
		}

		go ctx.Manager.Serve(ln, func(c net.Conn) {
			opts, ok := <-optsOut
			if !ok {
				return
			}

			cc := netutil.NewConnChecker(c)
			defer cc.Close()

			var dnsMsg dns.Msg

			for {
				dnsMsg.MsgHeader = dns.MsgHeader{}
				dnsMsg.Question = nil
				dnsMsg.Reset()

				_, err := dnsMsg.ReadFrom(cc)
				if err == nil {
					err = dnsMsg.Unpack()
				}
				if err != nil {
					if err != io.EOF {
						logger.Debug("readmsg", slog.Any("error", err))
					}
					return
				}
				if len(dnsMsg.Question) == 0 {
					logger.Debug("readmsg", slog.String("error", "0 questions"))
					return
				}

				z, t := dnsutil.Question(&dnsMsg)

				var qr *dnsQueryResult

				if opts.IPv4.Only && t == dns.TypeAAAA {
					qr = &dnsQueryResult{Message: dnsutil.SetReply(new(dns.Msg), &dnsMsg)}
				}

				if qr == nil && opts.dnsCache != nil {
					if cache, ok := opts.dnsCache.Load(dnsCacheKey{z, t}); ok {
						r := cache.(*dnsQueryResult)
						if r.Deadline.After(time.Now()) {
							qr = r
							if len(r.IPList) != 0 {
								logger.Debug("dnsquery",
									slog.String("host", strings.TrimSuffix(z, ".")),
									slog.Any("ip", r.IPList),
									slog.Duration("ttl", r.TTL()),
									slog.String("from", "cache"))
							}
						}
					}
				}

				if qr == nil {
					r := make(chan *dnsQueryResult, 1)

					select {
					case <-ctx.Done():
						return
					case <-cc.Done():
						return
					case dnsQueryIn <- dnsQuery{&dnsMsg, z, t, r, cc}:
					}

					select {
					case <-ctx.Done():
						return
					case <-cc.Done():
						return
					case qr = <-r:
					}

					if qr == nil {
						return
					}
				}

				m := qr.Message.Copy()
				m.ID = dnsMsg.ID
				m.Data = dnsMsg.Data
				if err := m.Pack(); err != nil {
					logger.Debug("packmsg", slog.Any("error", err))
					return
				}
				dnsMsg.Data = m.Data

				if n := uint32(time.Since(qr.Time).Seconds()); n > 0 {
					if err := dnsMsg.Unpack(); err != nil {
						logger.Debug("unpackmsg", slog.Any("error", err))
						return
					}
					for rr := range dnsMsg.RRs() {
						if h := rr.Header(); h != nil && h.TTL != 0 {
							if h.TTL > n {
								h.TTL -= n
							} else {
								h.TTL = 0
							}
						}
					}
					if err := dnsMsg.Pack(); err != nil {
						logger.Debug("packmsg", slog.Any("error", err))
						return
					}
				}

				if err := writeMsgData(cc, dnsMsg.Data); err != nil {
					logger.Debug("writemsg", slog.Any("error", err))
					return
				}
			}
		})

		return nil
	}

	stopServer := func() {
		if server == nil {
			return
		}

		defer logger.Info("net:listen:close", slog.Any("addr", server.Addr()))

		_ = server.Close()
		server = nil
	}
	defer stopServer()

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ctx.Event:
			switch ev := ev.(type) {
			case chrome.LoadEvent:
				old := <-optsOut
				new := *ev.Options.(*Options)
				new.routes = old.routes
				new.routeCache = old.routeCache
				new.allowListMap = old.allowListMap
				new.dnsCache = old.dnsCache

				if _, _, err := net.SplitHostPort(new.ListenAddr); err != nil {
					logger.Error("loading", slog.Any("error", err))
					return
				}

				if new.ListenAddr != old.ListenAddr {
					stopServer()
				}

				checkServers := func(servers []DNServer, route string) {
					invalid := func(i int, msg string) {
						if route == "" {
							logger.Error("loading:checkservers",
								slog.Int("index", i),
								slog.String("error", msg))
						} else {
							logger.Error("loading:checkservers",
								slog.String("route", route),
								slog.Int("index", i),
								slog.String("error", msg))
						}
						runtime.Goexit()
					}
					for i := range servers {
						server := &servers[i]
						server.Over = strings.ToUpper(server.Over)
						server.Method = strings.ToUpper(server.Method)
						if server.Name == "" && len(server.IP) == 0 {
							invalid(i, "missing name or IP address")
						}
						if !slices.Contains([]string{"", "TCP", "TLS", "HTTPS", "HTTP"}, server.Over) {
							invalid(i, "invalid over field: expects TCP, TLS, HTTPS or HTTP")
						}
						if !slices.Contains([]string{"", "GET", "POST"}, server.Method) {
							invalid(i, "invalid method field: expects GET or POST")
						}
						if (server.Over == "TLS" || server.Over == "HTTPS") && server.Name == "" {
							invalid(i, "DNS-over-TLS/HTTPS requires a server name")
						}
					}
				}

				if len(new.Servers) == 0 && (new.Server.Name != "" || len(new.Server.IP) != 0) {
					new.Servers = append(new.Servers, new.Server)
				}

				checkServers(new.Servers, "")

				for i := range new.Routes {
					r := &new.Routes[i]

					if r.Name == "" {
						if advance, token, _ := scanLines([]byte(r.What), true); advance == len(r.What) {
							r.Name = string(token)
						}
					}

					if r.Name == "" {
						r.Name = fmt.Sprintf("#%v", i)
					}

					sum, err := checksum(ctx.Manager, r.What)
					if err != nil {
						logger.Error("loading:checksum", slog.String("route", r.Name), slog.Any("error", err))
						return
					}

					r.Checksum = hex.EncodeToString(sum)

					if len(r.Servers) == 0 {
						logger.Error("loading:checkservers", slog.String("route", r.Name), slog.String("error", "no servers"))
						return
					}

					checkServers(r.Servers, r.Name)
				}

				if len(new.Servers) == 0 && len(new.Routes) == 0 {
					logger.Error("loading:checkservers", slog.String("error", "no servers"))
					return
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
								logger.Error("loading:initroute", slog.String("route", r.Name), slog.Any("error", err))
								return
							}
						}

						if new.allowListMap == nil {
							new.allowListMap = make(map[string]*allowList)
						}

						new.routes = append(new.routes, route{r.Name, l, r.Proxy, r.Servers})
						new.allowListMap[r.Name] = l
					}

					if new.routes != nil {
						new.routeCache = &sync.Map{}
					}
				}

				if new.Cache != old.Cache {
					new.dnsCache = nil
				}

				if new.Cache {
					if new.dnsCache == nil || shouldResetDNSCache(old, new) {
						new.dnsCache = &sync.Map{}
					}
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

type dnsQuery struct {
	Message *dns.Msg
	Fqdn    string
	Qtype   uint16
	Result  chan<- *dnsQueryResult
	Context context.Context
}

type dnsQueryResult struct {
	Message  *dns.Msg
	IPList   []netip.Addr
	Time     time.Time
	Deadline time.Time
}

func (r *dnsQueryResult) TTL() time.Duration {
	return time.Until(r.Deadline).Truncate(time.Second)
}

func startWorker(ctx chrome.Context, logger *slog.Logger, options <-chan Options, incoming <-chan dnsQuery) {
	var sex []*dnsExchange
	for {
		select {
		case <-ctx.Done():
			return
		case q := <-incoming:
			opts, ok := <-options
			if !ok {
				return
			}

			domain := strings.TrimSuffix(q.Fqdn, ".")

			if opts.dnsCache != nil {
				if cache, ok := opts.dnsCache.Load(dnsCacheKey{q.Fqdn, q.Qtype}); ok {
					r := cache.(*dnsQueryResult)
					if r.Deadline.After(time.Now()) {
						if len(r.IPList) != 0 {
							logger.Debug("dnsquery",
								slog.String("host", domain),
								slog.Any("ip", r.IPList),
								slog.Duration("ttl", r.TTL()),
								slog.String("from", "cache"))
						}

						select {
						case <-ctx.Done():
							return
						case <-q.Context.Done():
						case q.Result <- r:
						}

						close(q.Result)

						continue
					}
				}
			}

			var route1 *route

			if opts.routeCache != nil {
				if r, ok := opts.routeCache.Load(domain); ok {
					route1 = r.(*route)
				} else {
					dn := domain
					if v := opts.Redirects[dn]; v != "" {
						dn = v
					}
					for i := range opts.routes {
						if r := &opts.routes[i]; r.AllowList.Allow(dn) {
							logger.Info("route:match", slog.String("route", r.Name), slog.String("matches", dn))
							route1 = r
							break
						}
					}
					opts.routeCache.Store(domain, route1)
				}
			}

			if i := slices.IndexFunc(sex, func(ex *dnsExchange) bool { return ex.Route == route1 }); i >= 0 {
				ex := sex[i]
				select {
				case <-ex.Done():
				case <-q.Context.Done():
					continue
				case ex.Chan <- q:
					continue
				}
				j := len(sex) - 1
				sex[i] = sex[j]
				sex[j] = nil
				sex = sex[:j]
			}

			// Remove inactive exchanges.
			sex = slices.DeleteFunc(sex, func(ex *dnsExchange) bool {
				select {
				case <-ex.Done():
					return true
				default:
					return false
				}
			})

			ex := newExchange(ctx, logger, options, route1)
			sex = append(sex, ex)

			select {
			case <-ex.Done():
			case <-q.Context.Done():
			case ex.Chan <- q:
			}
		}
	}
}

type dnsExchange struct {
	context.Context
	Route *route
	Chan  chan dnsQuery
}

func newExchange(ctx chrome.Context, logger *slog.Logger, options <-chan Options, route *route) *dnsExchange {
	ctx1, cancel := context.WithCancel(ctx.Context)
	ex := &dnsExchange{
		Context: ctx1,
		Route:   route,
		Chan:    make(chan dnsQuery),
	}

	go func() {
		defer cancel()
		startExchange(ctx, logger, options, ex)
	}()

	return ex
}

func startExchange(ctx chrome.Context, logger *slog.Logger, options <-chan Options, ex *dnsExchange) {
	var (
		dnsConn struct {
			sync.Mutex
			sync.WaitGroup
			Net          net.Conn
			Chrome       *chrome.Conn
			ReadTimeout  time.Duration
			WriteTimeout time.Duration
			GotResponse  bool
		}
		dnsMsgData []byte
		dnsServer  DNServer
		dnsAttrs   []slog.Attr
	)

	type keyType struct{ s1, s2 string }

	var (
		tlsConfig      *tls.Config
		tlsConfigKey   keyType
		tlsConfigCache map[keyType]*tls.Config
	)

	httpProtocols := &http.Protocols{}
	httpProtocols.SetHTTP1(true)
	httpProtocols.SetHTTP2(true)

	httpTransport := &http.Transport{
		DialTLSContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			if dnsConn.Net == nil {
				return nil, errors.New("internal error: connection wasn't created")
			}
			return dnsConn.Net, nil
		},
		Protocols: httpProtocols,
	}

	closeConnection := func() {
		if dnsConn.Net != nil {
			dnsConn.Net.Close()
			dnsConn.Wait()
			dnsConn.Net = nil
			dnsConn.Chrome = nil
			dnsConn.GotResponse = false
		}
		httpTransport.CloseIdleConnections()
	}

	defer closeConnection()

	idleTimeout := defaultIdleTimeout
	idleTimer := time.NewTimer(idleTimeout)
	defer idleTimer.Stop()

	idleTimerC := idleTimer.C

	for first := true; ; first = false {
		_ = first || idleTimer.Reset(idleTimeout)
		select {
		case <-ctx.Done():
			return
		case <-idleTimerC:
			return
		case q := <-ex.Chan:
			opts, ok := <-options
			if !ok {
				return
			}

			var result *dnsQueryResult

			domain := strings.TrimSuffix(q.Fqdn, ".")

			if opts.dnsCache != nil {
				if cache, ok := opts.dnsCache.Load(dnsCacheKey{q.Fqdn, q.Qtype}); ok {
					r := cache.(*dnsQueryResult)
					if r.Deadline.After(time.Now()) {
						result = r
						if len(r.IPList) != 0 {
							logger.Debug("dnsquery",
								slog.String("host", domain),
								slog.Any("ip", r.IPList),
								slog.Duration("ttl", r.TTL()),
								slog.String("from", "cache"))
						}
					}
				}
			}

			for result == nil {
				if ctx.Err() != nil {
					return
				}

				if q.Context.Err() != nil {
					break
				}

				if dnsConn.Net == nil {
					opts, ok = <-options
					if !ok {
						return
					}

					servers := opts.Servers
					if ex.Route != nil {
						servers = ex.Route.Servers
					}

					if len(servers) == 0 {
						logger.Error("dnsquery", slog.String("error", "no servers"))
						return
					}

					dnsServer = servers[rand.IntN(len(servers))]
					dnsAttrs = append(dnsAttrs[:0], slog.String("name", dnsServer.Name))

					host := dnsServer.Name
					if len(dnsServer.IP) != 0 {
						host = dnsServer.IP[rand.IntN(len(dnsServer.IP))]
						dnsAttrs = append(dnsAttrs, slog.String("ip", host))
					}

					port := dnsServer.port()
					hostport := net.JoinHostPort(host, strconv.Itoa(port))
					dnsAttrs = append(dnsAttrs, slog.Int("port", port))

					getRemote := func(ctx context.Context) (net.Conn, error) {
						if q.Context.Err() != nil {
							return nil, chrome.CloseConn
						}

						opts, ok := <-options
						if !ok {
							return nil, chrome.CloseConn
						}

						if ex.Route != nil {
							opts.Proxy = ex.Route.Proxy
						}

						ctx, cancel := context.WithCancel(ctx)
						defer cancel()

						stop := context.AfterFunc(q.Context, cancel)
						defer stop()

						return proxy.Dial(ctx, opts.Proxy.Dialer(), "tcp", hostport)
					}

					dnsConn.Lock()
					dnsConn.Add(1)

					chromeConn := ctx.Manager.NewConn(hostport, getRemote, opts.Conn, opts.Relay, logger, func(c <-chan chrome.ConnEvent) {
						defer dnsConn.Done()
						for ev := range c {
							if _, ok := ev.(chrome.ConnEventMaxAttempts); ok {
								aLongTimeAgo := time.Unix(1, 0)
								dnsConn.Net.SetDeadline(aLongTimeAgo)
							}
						}
					})
					netConn := net.Conn(chromeConn)

					if dnsServer.Over == "TLS" || dnsServer.Over == "HTTPS" {
						key := keyType{dnsServer.Name, dnsServer.Over}
						if key != tlsConfigKey {
							tlsConfig, tlsConfigKey = tlsConfigCache[key], key
							if tlsConfig == nil {
								tlsConfig = &tls.Config{
									ServerName:         dnsServer.Name,
									ClientSessionCache: tls.NewLRUClientSessionCache(1),
								}
								if dnsServer.Over == "HTTPS" {
									tlsConfig.NextProtos = dnshttp.NextProtos
								}
								if tlsConfigCache == nil {
									tlsConfigCache = make(map[keyType]*tls.Config)
								}
								tlsConfigCache[key] = tlsConfig
							}
						}
						netConn = tls.Client(netConn, tlsConfig)
						dnsAttrs = append(dnsAttrs, slog.String("over", dnsServer.Over))
					}

					dnsConn.Net = netConn
					dnsConn.Chrome = chromeConn

					idleTimeout = defaultIdleTimeout
					dnsConn.ReadTimeout = defaultReadTimeout
					dnsConn.WriteTimeout = defaultWriteTimeout

					if opts.Idle.Timeout > 0 {
						idleTimeout = opts.Idle.Timeout
					}
					if opts.Read.Timeout > 0 {
						dnsConn.ReadTimeout = opts.Read.Timeout
					}
					if opts.Write.Timeout > 0 {
						dnsConn.WriteTimeout = opts.Write.Timeout
					}

					dnsConn.Unlock()
				}

				var dnsMsg dns.Msg

				dnsMsg.Data = dnsMsgData

				z := q.Fqdn
				if v := opts.Redirects[domain]; v != "" {
					z = dnsutil.Fqdn(v)
				}
				dnsutil.SetQuestion(&dnsMsg, z, q.Qtype)

				err := func() error {
					if dnsServer.Over == "HTTPS" || dnsServer.Over == "HTTP" {
						req, err := dnshttp.NewRequest(dnsServer.method(), dnsServer.url(), &dnsMsg)
						if err != nil {
							logger.Debug("dnsquery:newreq",
								slog.String("host", domain),
								slog.GroupAttrs("dns", dnsAttrs...),
								slog.Any("error", err))
							return err
						}
						if dnsServer.Path != "" {
							req.URL.Path = path.Join("/", dnsServer.Path)
						}

						dnsConn.Chrome.SetOutgoingTimeout(dnsConn.ReadTimeout, dnsConn.WriteTimeout)

						resp, err := httpTransport.RoundTrip(req)
						if err != nil {
							logger.Debug("dnsquery:roundtrip",
								slog.String("host", domain),
								slog.GroupAttrs("dns", dnsAttrs...),
								slog.Any("error", err))
							return err
						}

						m, err := dnshttp.Response(resp)
						if err != nil {
							logger.Debug("dnsquery:unpackresp",
								slog.String("host", domain),
								slog.GroupAttrs("dns", dnsAttrs...),
								slog.Any("error", err))
							return err
						}

						if cap(dnsMsg.Data) <= cap(m.Data) {
							dnsMsg.Data = m.Data
						} else {
							dnsMsg.Data = append(dnsMsg.Data[:0], m.Data...)
						}
					} else {
						if err := dnsMsg.Pack(); err != nil {
							logger.Debug("dnsquery:packmsg",
								slog.String("host", domain),
								slog.GroupAttrs("dns", dnsAttrs...),
								slog.Any("error", err))
							return err
						}

						dnsConn.Chrome.SetOutgoingTimeout(dnsConn.ReadTimeout, dnsConn.WriteTimeout)

						if err := writeMsgData(dnsConn.Net, dnsMsg.Data); err != nil {
							logger.Debug("dnsquery:writemsg",
								slog.String("host", domain),
								slog.GroupAttrs("dns", dnsAttrs...),
								slog.Any("error", err))
							return err
						}

						dnsConn.Chrome.SetOutgoingTimeout(dnsConn.ReadTimeout, dnsConn.WriteTimeout)

						if _, err := dnsMsg.ReadFrom(dnsConn.Net); err != nil {
							logger.Debug("dnsquery:readmsg",
								slog.String("host", domain),
								slog.GroupAttrs("dns", dnsAttrs...),
								slog.Any("error", err))
							return err
						}
					}

					dnsConn.Chrome.SetOutgoingTimeout(0, 0)

					if err := dnsMsg.Unpack(); err != nil {
						logger.Debug("dnsquery:unpackmsg",
							slog.String("host", domain),
							slog.GroupAttrs("dns", dnsAttrs...),
							slog.Any("error", err))
						return err
					}

					r := &dnsQueryResult{Message: &dnsMsg}

					if z != q.Fqdn {
						dnsMsg.Question = q.Message.Question
					}

					var minTTL uint32 = math.MaxUint32

					var excludeIPv6 bool

					for rr := range dnsMsg.RRs() {
						if h := rr.Header(); h != nil {
							if h.Name == z {
								h.Name = q.Fqdn
							}
							if opts.TTL.Min > 0 || opts.TTL.Max > 0 {
								ttl := time.Duration(h.TTL) * time.Second
								if opts.TTL.Min > 0 && ttl < opts.TTL.Min {
									ttl = opts.TTL.Min
								}
								if opts.TTL.Max > 0 && ttl > opts.TTL.Max {
									ttl = opts.TTL.Max
								}
								if ttl != time.Duration(h.TTL)*time.Second {
									h.TTL = uint32(math.Ceil(ttl.Seconds()))
								}
							}
							if minTTL > h.TTL {
								minTTL = h.TTL
							}
						}

						switch rr := rr.(type) {
						case *dns.A:
							if rr.Addr.IsValid() {
								r.IPList = append(r.IPList, rr.Addr)
							}
						case *dns.AAAA:
							if opts.IPv4.Only {
								excludeIPv6 = true
								break
							}
							if rr.Addr.IsValid() {
								r.IPList = append(r.IPList, rr.Addr)
							}
						}
					}

					if excludeIPv6 {
						dnsMsg.Answer = slices.DeleteFunc(dnsMsg.Answer, func(rr dns.RR) bool {
							_, ok := rr.(*dns.AAAA)
							return ok
						})
					}

					r.Time = time.Now()

					if minTTL < math.MaxUint32 {
						r.Deadline = r.Time.Add(time.Duration(minTTL) * time.Second)
					}

					result = r

					if opts.dnsCache != nil && !r.Deadline.IsZero() {
						opts.dnsCache.Store(dnsCacheKey{q.Fqdn, q.Qtype}, r)
					}

					if len(r.IPList) != 0 {
						logger.Debug("dnsquery",
							slog.String("host", domain),
							slog.Any("ip", r.IPList),
							slog.Duration("ttl", r.TTL()))
					}

					return nil
				}()

				dnsMsgData = dnsMsg.Data
				dnsMsg.Data = nil

				if err == nil {
					break
				}

				closeConnection()
			}

			if result != nil {
				select {
				case <-ctx.Done():
					return
				case <-q.Context.Done():
				case q.Result <- result:
				}
			}

			close(q.Result)
		}
	}
}

func shouldResetDNSCache(x, y Options) bool {
	return x.IPv4 != y.IPv4 || x.TTL != y.TTL ||
		!reflect.DeepEqual(&x.Servers, &y.Servers) ||
		!routesEqual(x.Routes, y.Routes) ||
		!reflect.DeepEqual(&x.Redirects, &y.Redirects)
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

func writeMsgData(w io.Writer, data []byte) error {
	s := make([]byte, 2)
	binary.BigEndian.PutUint16(s, uint16(len(data)))
	bw := bufio.NewWriter(w)
	_, err1 := bw.Write(s)
	_, err2 := bw.Write(data)
	return cmp.Or(err1, err2, bw.Flush())
}

const (
	defaultIdleTimeout  = 10 * time.Second
	defaultReadTimeout  = 2 * time.Second
	defaultWriteTimeout = 3 * time.Second
)

var rePortSuffix = regexp.MustCompile(`:\d+$`)
