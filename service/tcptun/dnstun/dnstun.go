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
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
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
	Name string
	IP   chrome.StringList
	Over string
	Port uint16
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
					for i := range servers {
						server := &servers[i]
						if server.Name == "" && len(server.IP) == 0 {
							if route == "" {
								logger.Error("loading:checkservers",
									slog.Int("index", i),
									slog.String("error", "missing name or IP address"))
							} else {
								logger.Error("loading:checkservers",
									slog.String("route", route),
									slog.Int("index", i),
									slog.String("error", "missing name or IP address"))
							}
							runtime.Goexit()
						}

						server.Over = strings.ToUpper(server.Over)

						if server.Over == "TLS" && server.Name == "" {
							if route == "" {
								logger.Error("loading:checkservers",
									slog.Int("index", i),
									slog.String("error", "DNS-over-TLS requires a server name"))
							} else {
								logger.Error("loading:checkservers",
									slog.String("route", route),
									slog.Int("index", i),
									slog.String("error", "DNS-over-TLS requires a server name"))
							}
							runtime.Goexit()
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
Loop:
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

						continue Loop
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

			for i, ex := range sex {
				if ex.Route == route1 {
					select {
					case <-ex.Done():
					case <-q.Context.Done():
						continue Loop
					case ex.Chan <- q:
						continue Loop
					}

					j := len(sex) - 1
					sex[i] = sex[j]
					sex[j] = nil
					sex = sex[:j]

					break
				}
			}

			{ // Remove inactive exchanges.
				s := sex
				sex = s[:0]

				for _, ex := range s {
					select {
					case <-ex.Done():
					default:
						sex = append(sex, ex)
					}
				}

				s = s[len(sex):]

				for i := range s {
					s[i] = nil
				}
			}

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
	var dnsConn struct {
		net.Conn
		sync.Mutex
		sync.WaitGroup
		Timeout     time.Duration
		Deadline    time.Time
		GotResponse bool
	}

	var dnsMsgData []byte

	var tlsConfig *tls.Config

	var tlsConfigCache map[string]*tls.Config

	setTimeout := func(d time.Duration) {
		dnsConn.Lock()
		dnsConn.Timeout = d
		dnsConn.Deadline = time.Now().Add(d)
		if dnsConn.GotResponse {
			dnsConn.SetDeadline(dnsConn.Deadline)
		}
		dnsConn.Unlock()
	}

	closeConnection := func() {
		dnsConn.Close()
		dnsConn.Wait()
		dnsConn.Conn = nil
		dnsConn.Timeout = 0
		dnsConn.Deadline = time.Time{}
		dnsConn.GotResponse = false
	}

	defer func() {
		if dnsConn.Conn != nil {
			closeConnection()
		}
	}()

	dnsConnIdleTimeout := defaultIdleTimeout
	dnsConnReadTimeout := defaultReadTimeout
	dnsConnWriteTimeout := defaultWriteTimeout

	var dnsAttrs []slog.Attr

	idleTimer := time.NewTimer(dnsConnIdleTimeout)
	defer idleTimer.Stop()

	idleTimerC := idleTimer.C

	for {
		if !idleTimer.Stop() {
			<-idleTimerC
		}

		idleTimer.Reset(dnsConnIdleTimeout)

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

				if dnsConn.Conn == nil {
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

					server := servers[rand.IntN(len(servers))]
					dnsAttrs = append(dnsAttrs[:0], slog.String("name", server.Name))

					host := server.Name
					if len(server.IP) != 0 {
						host = server.IP[rand.IntN(len(server.IP))]
						dnsAttrs = append(dnsAttrs, slog.String("ip", host))
					}

					port := 53
					if server.Over == "TLS" {
						port = 853
					}
					if server.Port != 0 {
						port = int(server.Port)
					}

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

					conn := ctx.Manager.NewConn(hostport, getRemote, opts.Conn, opts.Relay, logger, func(c <-chan chrome.ConnEvent) {
						defer dnsConn.Done()
						for ev := range c {
							switch ev.(type) {
							case chrome.ConnEventResponse:
								dnsConn.Lock()
								dnsConn.GotResponse = true
								if dnsConn.Timeout != 0 {
									dnsConn.Deadline = time.Now().Add(dnsConn.Timeout)
									dnsConn.SetDeadline(dnsConn.Deadline)
								}
								dnsConn.Unlock()
							case chrome.ConnEventMaxAttempts:
								aLongTimeAgo := time.Unix(1, 0)
								dnsConn.SetDeadline(aLongTimeAgo)
							}
						}
					})

					if server.Over == "TLS" {
						if tlsConfig == nil || tlsConfig.ServerName != server.Name {
							tlsConfig = tlsConfigCache[server.Name]
							if tlsConfig == nil {
								tlsConfig = &tls.Config{
									ServerName:         server.Name,
									ClientSessionCache: tls.NewLRUClientSessionCache(1),
								}

								if tlsConfigCache == nil {
									tlsConfigCache = make(map[string]*tls.Config)
								}

								tlsConfigCache[server.Name] = tlsConfig
							}
						}

						conn = tls.Client(conn, tlsConfig)
						dnsAttrs = append(dnsAttrs, slog.String("over", "TLS"))
					}

					dnsConn.Conn = conn

					dnsConnIdleTimeout = defaultIdleTimeout
					dnsConnReadTimeout = defaultReadTimeout
					dnsConnWriteTimeout = defaultWriteTimeout

					if opts.Idle.Timeout > 0 {
						dnsConnIdleTimeout = opts.Idle.Timeout
					}

					if opts.Read.Timeout > 0 {
						dnsConnReadTimeout = opts.Read.Timeout
					}

					if opts.Write.Timeout > 0 {
						dnsConnWriteTimeout = opts.Write.Timeout
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

				setTimeout(dnsConnWriteTimeout)

				err := dnsMsg.Pack()
				if err == nil {
					err = writeMsgData(dnsConn.Conn, dnsMsg.Data)
				}
				if err == nil {
					setTimeout(dnsConnReadTimeout)

					_, err = dnsMsg.ReadFrom(dnsConn.Conn)
					if err == nil {
						err = dnsMsg.Unpack()
					}
					if err == nil {
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

						break
					}

					logger.Debug("dnsquery:readmsg",
						slog.String("host", domain),
						slog.GroupAttrs("dns", dnsAttrs...),
						slog.Any("error", err))
				} else {
					logger.Debug("dnsquery:writemsg",
						slog.String("host", domain),
						slog.GroupAttrs("dns", dnsAttrs...),
						slog.Any("error", err))
				}

				dnsMsgData = dnsMsg.Data
				dnsMsg.Data = nil

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
