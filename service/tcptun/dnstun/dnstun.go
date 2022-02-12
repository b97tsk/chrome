package dnstun

import (
	"bufio"
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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/b97tsk/chrome"
	"github.com/b97tsk/chrome/internal/matchset"
	"github.com/b97tsk/chrome/internal/netutil"
	"github.com/miekg/dns"
)

type Options struct {
	ListenAddr string `yaml:"on"`

	Proxy chrome.Proxy `yaml:"over"`

	Server  DNServer
	Servers []DNServer

	Routes []RouteOptions

	Cache bool

	Dial struct {
		Timeout time.Duration
	}
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
	AllowList chrome.EnvString `yaml:"file"`
	Proxy     chrome.Proxy     `yaml:"over"`
	Servers   []DNServer

	AllowListChecksum string `yaml:"-"`
}

func (r *RouteOptions) equal(other *RouteOptions) bool {
	return r.AllowList == other.AllowList &&
		r.AllowListChecksum == other.AllowListChecksum &&
		r.Proxy.Equal(other.Proxy) &&
		reflect.DeepEqual(r.Servers, other.Servers)
}

type route struct {
	AllowList *allowList
	Proxy     chrome.Proxy
	Servers   []DNServer
}

type allowList struct {
	Path     string
	Checksum string

	includes matchset.MatchSet
	excludes matchset.MatchSet
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
			// portSuffix = ""
			pattern = line
		}

		if !exclude {
			l.includes.Add(pattern, struct{}{})
		} else {
			l.excludes.Add(pattern, struct{}{})
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

func (l *allowList) Allow(domain string) bool {
	return l.includes.MatchAll(domain) != nil && l.excludes.MatchAll(domain) == nil
}

type Service struct{}

const ServiceName = "dnstun"

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

			local, localCtx := netutil.NewConnChecker(c)
			defer local.Close()

			dnsConn := &dns.Conn{Conn: local}

			for {
				msg, err := dnsConn.ReadMsg()
				if err != nil {
					if err != io.EOF {
						logger.Tracef("(local) read msg: %v", err)
					}

					return
				}

				var result *dns.Msg

				if opts.dnsCache != nil {
					var qtype uint16

					if len(msg.Question) != 0 {
						qtype = msg.Question[0].Qtype
					}

					if qtype == dns.TypeA || qtype == dns.TypeAAAA {
						domain := strings.TrimSuffix(msg.Question[0].Name, ".")
						if cache, ok := opts.dnsCache.Load(domain); ok {
							r := cache.(*dnsQueryResult)
							if r.Deadline.After(time.Now()) {
								result = r.Message
								logger.Tracef("(from cache) %v: %v TTL=%v", domain, r.IPList, r.TTL())
							}
						}
					}
				}

				if result == nil {
					var qtype uint16

					var domain string

					if len(msg.Question) != 0 {
						qtype = msg.Question[0].Qtype
						domain = strings.TrimSuffix(msg.Question[0].Name, ".")
					}

					r := make(chan *dns.Msg, 1)

					select {
					case <-ctx.Done():
						return
					case <-localCtx.Done():
						return
					case dnsQueryIn <- dnsQuery{msg, qtype, domain, r, localCtx}:
					}

					select {
					case <-ctx.Done():
						return
					case <-localCtx.Done():
						return
					case result = <-r:
					}

					if result == nil {
						return
					}
				}

				if err := dnsConn.WriteMsg(result); err != nil {
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
		case opts := <-ctx.Load:
			if new, ok := opts.(*Options); ok {
				old := <-optsOut
				new := *new
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
								panic(nil)
							}

							logger.Errorf("server #%v: invalid", i+1)
							panic(nil)
						}

						server.Over = strings.ToUpper(server.Over)

						if server.Over == "TLS" && server.Name == "" {
							if routeIndex >= 0 {
								logger.Errorf("route #%v: server #%v: DNS-over-TLS requires a server name", routeIndex+1, i+1)
								panic(nil)
							}

							logger.Errorf("server #%v: DNS-over-TLS requires a server name", i+1)
							panic(nil)
						}
					}
				}

				if len(new.Servers) == 0 && (new.Server.Name != "" || len(new.Server.IP) != 0) {
					new.Servers = append(new.Servers, new.Server)
				}

				checkServers(new.Servers, -1)

				for i := range new.Routes {
					r := &new.Routes[i]
					checksum1, _ := checksum(ctx.Manager, r.AllowList.String())
					r.AllowListChecksum = hex.EncodeToString(checksum1)

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

						new.routes = append(new.routes, route{l, r.Proxy, r.Servers})
						new.allowListMap[r.AllowList.String()] = l
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
			}
		case <-ctx.Loaded:
			if err := startServer(); err != nil {
				return
			}
		}
	}
}

type dnsQuery struct {
	Message *dns.Msg
	Qtype   uint16
	Domain  string
	Result  chan<- *dns.Msg
	Context context.Context
}

type dnsQueryResult struct {
	Message  *dns.Msg
	IPList   []net.IP
	Deadline time.Time
}

func (r *dnsQueryResult) TTL() time.Duration {
	return time.Until(r.Deadline).Truncate(time.Second)
}

func startWorker(ctx chrome.Context, options <-chan Options, incoming <-chan dnsQuery) {
	var transactions []*transaction

	logger := ctx.Manager.Logger(ServiceName)
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

			if opts.dnsCache != nil && (q.Qtype == dns.TypeA || q.Qtype == dns.TypeAAAA) {
				if cache, ok := opts.dnsCache.Load(q.Domain); ok {
					r := cache.(*dnsQueryResult)
					if r.Deadline.After(time.Now()) {
						logger.Tracef("(from cache) %v: %v TTL=%v", q.Domain, r.IPList, r.TTL())

						select {
						case <-ctx.Done():
							return
						case <-q.Context.Done():
						case q.Result <- r.Message:
						default:
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
					for i := range opts.routes {
						if r := &opts.routes[i]; r.AllowList.Allow(q.Domain) {
							logger.Infof("%v matches %v", r.AllowList.Path, q.Domain)

							route1 = r

							break
						}
					}

					opts.routeCache.Store(q.Domain, route1)
				}
			}

			for i, tr := range transactions {
				if tr.Route == route1 {
					select {
					case <-tr.Done():
					case tr.Chan <- q:
						continue Loop
					}

					j := len(transactions) - 1
					transactions[i] = transactions[j]
					transactions[j] = nil
					transactions = transactions[:j]

					break
				}
			}

			tr := newTransaction(ctx, options, route1)
			transactions = append(transactions, tr)

			select {
			case <-tr.Done():
			case tr.Chan <- q:
			}
		}
	}
}

type transaction struct {
	context.Context
	Route *route
	Chan  chan dnsQuery
}

func newTransaction(ctx chrome.Context, options <-chan Options, route *route) *transaction {
	ctx1, cancel := context.WithCancel(ctx.Context)
	tr := &transaction{
		Context: ctx1,
		Route:   route,
		Chan:    make(chan dnsQuery),
	}

	go func() {
		defer cancel()
		startTransaction(ctx, options, tr)
	}()

	return tr
}

func startTransaction(ctx chrome.Context, options <-chan Options, tr *transaction) {
	var dnsConn *dns.Conn

	defer func() {
		if dnsConn != nil {
			dnsConn.Close()
		}
	}()

	var tlsConfig *tls.Config

	var tlsConfigCache map[string]*tls.Config

	logger := ctx.Manager.Logger(ServiceName)

	dnsConnIdleTimeout := defaultIdleTimeout
	dnsConnReadTimeout := defaultReadTimeout
	dnsConnWriteTimeout := defaultWriteTimeout

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
		case q := <-tr.Chan:
			opts, ok := <-options
			if !ok {
				return
			}

			var result *dnsQueryResult

			if opts.dnsCache != nil && (q.Qtype == dns.TypeA || q.Qtype == dns.TypeAAAA) {
				if cache, ok := opts.dnsCache.Load(q.Domain); ok {
					r := cache.(*dnsQueryResult)
					if r.Deadline.After(time.Now()) {
						result = r
						logger.Tracef("(from cache) %v: %v TTL=%v", q.Domain, r.IPList, r.TTL())
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

				if dnsConn == nil {
					opts, ok = <-options
					if !ok {
						return
					}

					servers := opts.Servers
					proxy := &opts.Proxy

					if tr.Route != nil {
						servers = tr.Route.Servers
						proxy = &tr.Route.Proxy
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

					conn, err := ctx.Manager.Dial(q.Context, proxy.Dialer(), "tcp", hostport, opts.Dial.Timeout)
					if err != nil {
						if es := ""; !canceled(err, &es) {
							logger.Tracef("dial %v: %v", hostport, es)
						}

						break
					}

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

					dnsConn = &dns.Conn{Conn: conn}

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
				}

				_ = dnsConn.SetDeadline(time.Now().Add(dnsConnWriteTimeout))

				err := dnsConn.WriteMsg(q.Message)
				if err == nil {
					var msg *dns.Msg

					_ = dnsConn.SetDeadline(time.Now().Add(dnsConnReadTimeout))

					msg, err = dnsConn.ReadMsg()
					if err == nil {
						r := dnsQueryResult{Message: msg}

						for _, ans := range msg.Answer {
							if h := ans.Header(); h != nil {
								ttl := time.Duration(h.Ttl) * time.Second
								if opts.TTL.Min > 0 || opts.TTL.Max > 0 {
									origin := ttl

									if opts.TTL.Min > 0 && ttl < opts.TTL.Min {
										ttl = opts.TTL.Min
									}

									if opts.TTL.Max > 0 && ttl > opts.TTL.Max {
										ttl = opts.TTL.Max
									}

									if ttl != origin {
										h.Ttl = uint32(math.Ceil(ttl.Seconds()))
									}
								}

								if r.Deadline.IsZero() {
									r.Deadline = time.Now().Add(ttl)
								}
							}

							switch ans := ans.(type) {
							case *dns.A:
								r.IPList = append(r.IPList, ans.A)
							case *dns.AAAA:
								r.IPList = append(r.IPList, ans.AAAA)
							}
						}

						result = &r

						if len(r.IPList) != 0 {
							if opts.dnsCache != nil {
								opts.dnsCache.Store(q.Domain, &r)
							}

							logger.Debugf("(remote) %v: %v TTL=%v", q.Domain, r.IPList, r.TTL())
						}

						break
					}

					logger.Tracef("(remote) read msg: %v", err)
				} else {
					logger.Tracef("(remote) write msg: %v", err)
				}

				if err != nil {
					dnsConn.Close()
					dnsConn = nil
				}
			}

			if result != nil {
				select {
				case <-ctx.Done():
					return
				case <-q.Context.Done():
				case q.Result <- result.Message:
				default:
				}
			}

			close(q.Result)
		}
	}
}

func shouldResetDNSCache(x, y Options) bool {
	return x.TTL != y.TTL || !reflect.DeepEqual(&x.Servers, &y.Servers) || !routesEqual(x.Routes, y.Routes)
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

func canceled(e error, es *string) bool {
	if e == context.Canceled {
		return true
	}

	*es = e.Error()

	return strings.Contains(*es, "operation was canceled")
}

const (
	defaultIdleTimeout  = 10 * time.Second
	defaultReadTimeout  = 2 * time.Second
	defaultWriteTimeout = 3 * time.Second
)

var rePortSuffix = regexp.MustCompile(`:\d+$`)
