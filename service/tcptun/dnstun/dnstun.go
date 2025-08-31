package dnstun

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
	"math"
	"math/rand"
	"net"
	"path"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/b97tsk/chrome"
	"github.com/b97tsk/chrome/internal/matchset"
	"github.com/b97tsk/chrome/internal/netutil"
	"github.com/b97tsk/log"
	"github.com/b97tsk/proxy"
	"github.com/miekg/dns"
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

	return nil
}

type allowListParser struct {
	logger *log.Logger

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

func (l *allowList) Allow(domain string) bool {
	return l.includes.patterns.MatchAll(domain) != nil && l.excludes.patterns.MatchAll(domain) == nil
}

type dnsCacheKey struct {
	Domain string
	Qtype  uint16
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
	logger := ctx.Manager.Logger(ctx.JobName)

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
			logger.Error(err)
			return err
		}

		defer logger.Infof("listening on %v", ln.Addr())

		server = ln

		if dnsQueryIn == nil {
			dnsQueryIn = make(chan dnsQuery)

			go startWorker(ctx, optsOut, dnsQueryIn)
		}

		go ctx.Manager.Serve(ln, func(c net.Conn) {
			opts, ok := <-optsOut
			if !ok {
				return
			}

			cc := netutil.NewConnChecker(c)
			defer cc.Close()

			dnsConn := &dns.Conn{Conn: cc}

			for {
				msg, err := dnsConn.ReadMsg()
				if err != nil {
					if err != io.EOF {
						logger.Tracef("(local) read msg: %v", err)
					}

					return
				}

				if len(msg.Question) == 0 {
					logger.Trace("(local) read msg: 0 questions")
					return
				}

				domain := strings.TrimSuffix(msg.Question[0].Name, ".")
				qtype := msg.Question[0].Qtype

				var qr *dnsQueryResult

				if opts.IPv4.Only && qtype == dns.TypeAAAA {
					qr = &dnsQueryResult{Message: new(dns.Msg).SetReply(msg)}
				}

				if qr == nil && opts.dnsCache != nil {
					if cache, ok := opts.dnsCache.Load(dnsCacheKey{domain, qtype}); ok {
						r := cache.(*dnsQueryResult)
						if r.Deadline.After(time.Now()) {
							qr = r

							if len(r.IPList) != 0 {
								logger.Tracef("(from cache) %v: %v TTL=%v", domain, r.IPList, r.TTL())
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
					case dnsQueryIn <- dnsQuery{msg, domain, qtype, r, cc}:
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

				msgID := msg.Id
				qr.Message.CopyTo(msg)
				msg.Id = msgID

				if n := uint32(time.Since(qr.Time).Seconds()); n > 0 {
					for _, ans := range msg.Answer {
						if h := ans.Header(); h != nil && h.Ttl != 0 {
							if h.Ttl > n {
								h.Ttl -= n
							} else {
								h.Ttl = 0
							}
						}
					}
				}

				if err := dnsConn.WriteMsg(msg); err != nil {
					logger.Tracef("(local) write msg: %v", err)
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

		defer logger.Infof("stopped listening on %v", server.Addr())

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
					logger.Error(err)
					return
				}

				if new.ListenAddr != old.ListenAddr {
					stopServer()
				}

				checkServers := func(servers []DNServer, routeIndex int) {
					for i := range servers {
						server := &servers[i]
						if server.Name == "" && len(server.IP) == 0 {
							if routeIndex >= 0 {
								logger.Errorf("route #%v: server #%v: invalid", routeIndex+1, i+1)
								runtime.Goexit()
							}

							logger.Errorf("server #%v: invalid", i+1)
							runtime.Goexit()
						}

						server.Over = strings.ToUpper(server.Over)

						if server.Over == "TLS" && server.Name == "" {
							if routeIndex >= 0 {
								logger.Errorf("route #%v: server #%v: DNS-over-TLS requires a server name", routeIndex+1, i+1)
								runtime.Goexit()
							}

							logger.Errorf("server #%v: DNS-over-TLS requires a server name", i+1)
							runtime.Goexit()
						}
					}
				}

				if len(new.Servers) == 0 && (new.Server.Name != "" || len(new.Server.IP) != 0) {
					new.Servers = append(new.Servers, new.Server)
				}

				checkServers(new.Servers, -1)

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

					if len(r.Servers) == 0 {
						logger.Errorf("route #%v: no servers", i+1)
						return
					}

					checkServers(r.Servers, i)
				}

				if len(new.Servers) == 0 && len(new.Routes) == 0 {
					logger.Error("no servers")
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
								logger.Error(err)
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
	Domain  string
	Qtype   uint16
	Result  chan<- *dnsQueryResult
	Context context.Context
}

type dnsQueryResult struct {
	Message  *dns.Msg
	IPList   []net.IP
	Time     time.Time
	Deadline time.Time
}

func (r *dnsQueryResult) TTL() time.Duration {
	return time.Until(r.Deadline).Truncate(time.Second)
}

func startWorker(ctx chrome.Context, options <-chan Options, incoming <-chan dnsQuery) {
	var sex []*dnsExchange

	logger := ctx.Manager.Logger(ctx.JobName)
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

			if opts.dnsCache != nil {
				if cache, ok := opts.dnsCache.Load(dnsCacheKey{q.Domain, q.Qtype}); ok {
					r := cache.(*dnsQueryResult)
					if r.Deadline.After(time.Now()) {
						if len(r.IPList) != 0 {
							logger.Tracef("(from cache) %v: %v TTL=%v", q.Domain, r.IPList, r.TTL())
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
				if r, ok := opts.routeCache.Load(q.Domain); ok {
					route1 = r.(*route)
				} else {
					dm := q.Domain

					if v := opts.Redirects[dm]; v != "" {
						dm = v
					}

					for i := range opts.routes {
						if r := &opts.routes[i]; r.AllowList.Allow(dm) {
							logger.Infof("%v matches %v", r.Name, dm)

							route1 = r

							break
						}
					}

					opts.routeCache.Store(q.Domain, route1)
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

			ex := newExchange(ctx, options, route1)
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

func newExchange(ctx chrome.Context, options <-chan Options, route *route) *dnsExchange {
	ctx1, cancel := context.WithCancel(ctx.Context)
	ex := &dnsExchange{
		Context: ctx1,
		Route:   route,
		Chan:    make(chan dnsQuery),
	}

	go func() {
		defer cancel()
		startExchange(ctx, options, ex)
	}()

	return ex
}

func startExchange(ctx chrome.Context, options <-chan Options, ex *dnsExchange) {
	var dnsConn struct {
		*dns.Conn
		sync.Mutex
		sync.WaitGroup
		Timeout     time.Duration
		Deadline    time.Time
		GotResponse bool
	}

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

	idleTimer := time.NewTimer(dnsConnIdleTimeout)
	defer idleTimer.Stop()

	idleTimerC := idleTimer.C

	logger := ctx.Manager.Logger(ctx.JobName)

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

			if opts.dnsCache != nil {
				if cache, ok := opts.dnsCache.Load(dnsCacheKey{q.Domain, q.Qtype}); ok {
					r := cache.(*dnsQueryResult)
					if r.Deadline.After(time.Now()) {
						result = r

						if len(r.IPList) != 0 {
							logger.Tracef("(from cache) %v: %v TTL=%v", q.Domain, r.IPList, r.TTL())
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
						logger.Error("no servers")
						return
					}

					server := servers[rand.Intn(len(servers))]

					host := server.Name

					if len(server.IP) != 0 {
						host = server.IP[rand.Intn(len(server.IP))]
					}

					port := 53

					if server.Over == "TLS" {
						port = 853
					}

					if server.Port != 0 {
						port = int(server.Port)
					}

					hostport := net.JoinHostPort(host, strconv.Itoa(port))

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
					}

					dnsConn.Conn = &dns.Conn{Conn: conn}

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

				setTimeout(dnsConnWriteTimeout)

				qm := q.Message

				if v := opts.Redirects[q.Domain]; v != "" {
					qm = qm.Copy()
					qm.Question[0].Name = dns.Fqdn(v)
				}

				err := dnsConn.WriteMsg(qm)
				if err == nil {
					var msg *dns.Msg

					setTimeout(dnsConnReadTimeout)

					msg, err = dnsConn.ReadMsg()
					if err == nil {
						r := &dnsQueryResult{Message: msg}

						if qm != q.Message {
							msg.Question = q.Message.Question
						}

						var minTTL uint32 = math.MaxUint32

						var filterOutIPv6 bool

						for _, ans := range msg.Answer {
							if h := ans.Header(); h != nil {
								if qm != q.Message {
									h.Name = q.Message.Question[0].Name
								}

								if opts.TTL.Min > 0 || opts.TTL.Max > 0 {
									ttl := time.Duration(h.Ttl) * time.Second

									if opts.TTL.Min > 0 && ttl < opts.TTL.Min {
										ttl = opts.TTL.Min
									}

									if opts.TTL.Max > 0 && ttl > opts.TTL.Max {
										ttl = opts.TTL.Max
									}

									if ttl != time.Duration(h.Ttl)*time.Second {
										h.Ttl = uint32(math.Ceil(ttl.Seconds()))
									}
								}

								if minTTL > h.Ttl {
									minTTL = h.Ttl
								}
							}

							switch ans := ans.(type) {
							case *dns.A:
								r.IPList = append(r.IPList, ans.A)
							case *dns.AAAA:
								if opts.IPv4.Only {
									filterOutIPv6 = true
								} else {
									r.IPList = append(r.IPList, ans.AAAA)
								}
							}
						}

						if filterOutIPv6 {
							remains := msg.Answer[:0]

							for _, ans := range msg.Answer {
								if _, ok := ans.(*dns.AAAA); !ok {
									remains = append(remains, ans)
								}
							}

							for i, j := len(remains), len(msg.Answer); i < j; i++ {
								msg.Answer[i] = nil
							}

							msg.Answer = remains
						}

						r.Time = time.Now()

						if minTTL < math.MaxUint32 {
							r.Deadline = r.Time.Add(time.Duration(minTTL) * time.Second)
						}

						result = r

						if opts.dnsCache != nil && !r.Deadline.IsZero() {
							opts.dnsCache.Store(dnsCacheKey{q.Domain, q.Qtype}, r)
						}

						if len(r.IPList) != 0 {
							logger.Debugf("(remote) %v: %v TTL=%v", q.Domain, r.IPList, r.TTL())
						}

						break
					}

					logger.Tracef("(remote) read msg: %v", err)
				} else {
					logger.Tracef("(remote) write msg: %v", err)
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

const (
	defaultIdleTimeout  = 10 * time.Second
	defaultReadTimeout  = 2 * time.Second
	defaultWriteTimeout = 3 * time.Second
)

var rePortSuffix = regexp.MustCompile(`:\d+$`)
