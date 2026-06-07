package socks

import (
	"bufio"
	"cmp"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"path"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codeberg.org/miekg/dns"
	"codeberg.org/miekg/dns/dnshttp"
	"codeberg.org/miekg/dns/dnsutil"
	"github.com/b97tsk/chrome"
	"github.com/b97tsk/chrome/proxy"
	"github.com/shadowsocks/go-shadowsocks2/socks"
)

type Options struct {
	ListenAddr string `yaml:"on"`

	Proxy chrome.Proxy `yaml:"over"`

	Conn  chrome.ConnOptions
	Relay chrome.RelayOptions

	DNS struct {
		Server  DNServer
		Servers []DNServer

		Query struct {
			Type chrome.StringList
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

		Proxy *chrome.Proxy `yaml:"over"`
	}

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

type Service struct{}

const ServiceName = "socks"

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

		go ctx.Manager.Serve(ln, func(local net.Conn) {
			addr, err := socks.Handshake(local)
			if err != nil {
				return
			}

			opts, ok := <-optsOut
			if !ok {
				return
			}

			remoteAddr := addr.String()

			getRemote := func(localCtx context.Context) (net.Conn, error) {
				opts, ok := <-optsOut
				if !ok {
					return nil, chrome.CloseConn
				}

				getaddr1 := func() string { return remoteAddr }
				getaddr2 := (func() string)(nil)

				if len(opts.DNS.Servers) != 0 {
					if host, port, _ := net.SplitHostPort(remoteAddr); net.ParseIP(host) == nil {
						var result *dnsQueryResult

						if cache, ok := opts.dnsCache.Load(host); ok {
							r := cache.(*dnsQueryResult)
							if r.Deadline.After(time.Now()) {
								result = r
								logger.Debug("dnsquery",
									slog.String("host", host),
									slog.Any("ipv4", r.IPv4),
									slog.Any("ipv6", r.IPv6),
									slog.Duration("ttl", r.TTL()),
									slog.String("from", "cache"))
							}
						}

						if result == nil {
							r := make(chan *dnsQueryResult, 1)

							select {
							case <-ctx.Done():
								return nil, ctx.Err()
							case <-localCtx.Done():
								return nil, localCtx.Err()
							case dnsQueryIn <- dnsQuery{host, r, localCtx}:
							}

							select {
							case <-ctx.Done():
								return nil, ctx.Err()
							case <-localCtx.Done():
								return nil, localCtx.Err()
							case result = <-r:
							}

							if result == nil {
								return nil, errNoResult
							}
						}

						if result != nil {
							getaddr1, getaddr2 = nil, nil

							if ips := result.IPv4; len(ips) != 0 {
								getaddr1 = func() string {
									ip := ips[rand.IntN(len(ips))]
									return net.JoinHostPort(ip.String(), port)
								}
							}

							if ips := result.IPv6; len(ips) != 0 {
								getaddr2 = func() string {
									ip := ips[rand.IntN(len(ips))]
									return net.JoinHostPort(ip.String(), port)
								}
							}
						}
					}
				}

				if getaddr1 != nil && getaddr2 != nil {
					const N = 2

					type Context struct {
						Context context.Context
						Cancel  context.CancelFunc
					}

					var contexts [N]Context

					for i := range contexts {
						ctx, cancel := context.WithCancel(localCtx)
						contexts[i] = Context{ctx, cancel}
					}

					type Winner struct {
						Index int
						Conn  net.Conn
					}

					c := make(chan Winner)
					errCh := make(chan error, 1)

					var n atomic.Uint32

					for i, getaddr := range []func() string{getaddr1, getaddr2} {
						lctx := contexts[i].Context
						go func() {
							conn, err := proxy.Dial(lctx, opts.Proxy.Dialer(), "tcp", getaddr())
							if err != nil {
								select {
								case errCh <- err:
								default:
								}
							}
							if conn != nil {
								select {
								case c <- Winner{i, conn}:
								case <-lctx.Done():
									_ = conn.Close()
								}
							}
							if n.Add(1) == N {
								close(c)
							}
						}()
					}

					winner, ok := <-c
					if !ok {
						winner.Index = -1
					}

					for i := range contexts {
						if i != winner.Index {
							contexts[i].Cancel()
						}
					}

					if winner.Conn != nil {
						return winner.Conn, nil
					}

					return nil, <-errCh
				}

				switch {
				case getaddr1 != nil:
					return proxy.Dial(localCtx, opts.Proxy.Dialer(), "tcp", getaddr1())
				case getaddr2 != nil:
					return proxy.Dial(localCtx, opts.Proxy.Dialer(), "tcp", getaddr2())
				}

				return nil, errNoAddress
			}

			remote := ctx.Manager.NewConn(remoteAddr, getRemote, opts.Conn, opts.Relay, logger, nil)
			defer remote.Close()

			ctx.Manager.Relay(local, remote, opts.Relay)
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
				new.dnsCache = old.dnsCache

				if _, _, err := net.SplitHostPort(new.ListenAddr); err != nil {
					logger.Error("loading", slog.Any("error", err))
					return
				}

				if new.ListenAddr != old.ListenAddr {
					stopServer()
				}

				if len(new.DNS.Servers) == 0 && (new.DNS.Server.Name != "" || len(new.DNS.Server.IP) != 0) {
					new.DNS.Servers = append(new.DNS.Servers, new.DNS.Server)
				}

				if len(new.DNS.Servers) != 0 {
					new.DNS.Query.Type = normalizeQueryTypes(new.DNS.Query.Type)
					if len(new.DNS.Query.Type) == 0 {
						new.DNS.Query.Type = chrome.StringList{"A", "AAAA"}
					}

					for i := range new.DNS.Servers {
						server := &new.DNS.Servers[i]
						server.Over = strings.ToUpper(server.Over)
						server.Method = strings.ToUpper(server.Method)
						if server.Name == "" && len(server.IP) == 0 {
							logger.Error("loading:dns:checkservers",
								slog.Int("index", i),
								slog.String("error", "missing name or IP address"))
							return
						}
						if !slices.Contains([]string{"", "TCP", "TLS", "HTTPS", "HTTP"}, server.Over) {
							logger.Error("loading:dns:checkservers",
								slog.Int("index", i),
								slog.String("error", "invalid over field: expects TCP, TLS, HTTPS or HTTP"))
							return
						}
						if !slices.Contains([]string{"", "GET", "POST"}, server.Method) {
							logger.Error("loading:dns:checkservers",
								slog.Int("index", i),
								slog.String("error", "invalid method field: expects GET or POST"))
							return
						}
						if (server.Over == "TLS" || server.Over == "HTTPS") && server.Name == "" {
							logger.Error("loading:dns:checkservers",
								slog.Int("index", i),
								slog.String("error", "DNS-over-TLS/HTTPS requires a server name"))
							return
						}
					}

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
	Domain  string
	Result  chan<- *dnsQueryResult
	Context context.Context
}

type dnsQueryResult struct {
	IPv4, IPv6 []netip.Addr
	Deadline   time.Time
}

func (r *dnsQueryResult) TTL() time.Duration {
	return time.Until(r.Deadline).Truncate(time.Second)
}

func startWorker(ctx chrome.Context, logger *slog.Logger, options <-chan Options, incoming <-chan dnsQuery) {
	var (
		dnsConn struct {
			sync.Mutex
			sync.WaitGroup
			Net          net.Conn
			Chrome       *chrome.Conn
			IdleTimeout  time.Duration
			ReadTimeout  time.Duration
			WriteTimeout time.Duration
			GotResponse  bool
			IdleTimer    *time.Timer
			IdleTimerC   <-chan time.Time
		}
		dnsMsg    dns.Msg
		dnsServer DNServer
		dnsAttrs  []slog.Attr
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
			if dnsConn.IdleTimer != nil {
				dnsConn.IdleTimer.Stop()
				dnsConn.IdleTimer = nil
				dnsConn.IdleTimerC = nil
			}
		}
		httpTransport.CloseIdleConnections()
	}

	defer closeConnection()

	for {
		if dnsConn.Net != nil {
			if dnsConn.IdleTimer == nil {
				dnsConn.IdleTimer = time.NewTimer(dnsConn.IdleTimeout)
				dnsConn.IdleTimerC = dnsConn.IdleTimer.C
			} else {
				dnsConn.IdleTimer.Reset(dnsConn.IdleTimeout)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-dnsConn.IdleTimerC:
			closeConnection()
		case q := <-incoming:
			opts, ok := <-options
			if !ok {
				return
			}

			var result *dnsQueryResult

			if cache, ok := opts.dnsCache.Load(q.Domain); ok {
				r := cache.(*dnsQueryResult)
				if r.Deadline.After(time.Now()) {
					result = r
					logger.Debug("dnsquery",
						slog.String("host", q.Domain),
						slog.Any("ipv4", r.IPv4),
						slog.Any("ipv6", r.IPv6),
						slog.Duration("ttl", r.TTL()),
						slog.String("from", "cache"))
				}
			}

			if result == nil {
				r := &dnsQueryResult{}

				qtypes := opts.DNS.Query.Type
				for len(qtypes) != 0 {
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

						dnsServer = opts.DNS.Servers[rand.IntN(len(opts.DNS.Servers))]
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

							if opts.DNS.Proxy != nil {
								opts.Proxy = *opts.DNS.Proxy
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

						dnsConn.IdleTimeout = defaultIdleTimeout
						dnsConn.ReadTimeout = defaultReadTimeout
						dnsConn.WriteTimeout = defaultWriteTimeout

						if opts.DNS.Idle.Timeout > 0 {
							dnsConn.IdleTimeout = opts.DNS.Idle.Timeout
						}
						if opts.DNS.Read.Timeout > 0 {
							dnsConn.ReadTimeout = opts.DNS.Read.Timeout
						}
						if opts.DNS.Write.Timeout > 0 {
							dnsConn.WriteTimeout = opts.DNS.Write.Timeout
						}

						dnsConn.Unlock()
					}

					dnsMsg.MsgHeader = dns.MsgHeader{}
					dnsMsg.Question = nil
					dnsMsg.Reset()

					switch qtypes[0] {
					case "A":
						dnsutil.SetQuestion(&dnsMsg, dnsutil.Fqdn(q.Domain), dns.TypeA)
					case "AAAA":
						dnsutil.SetQuestion(&dnsMsg, dnsutil.Fqdn(q.Domain), dns.TypeAAAA)
					}

					err := func() error {
						if dnsServer.Over == "HTTPS" || dnsServer.Over == "HTTP" {
							req, err := dnshttp.NewRequest(dnsServer.method(), dnsServer.url(), &dnsMsg)
							if err != nil {
								logger.Debug("dnsquery:newreq",
									slog.String("host", q.Domain),
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
									slog.String("host", q.Domain),
									slog.GroupAttrs("dns", dnsAttrs...),
									slog.Any("error", err))
								return err
							}

							m, err := dnshttp.Response(resp)
							if err != nil {
								logger.Debug("dnsquery:unpackresp",
									slog.String("host", q.Domain),
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
									slog.String("host", q.Domain),
									slog.GroupAttrs("dns", dnsAttrs...),
									slog.Any("error", err))
								return err
							}

							dnsConn.Chrome.SetOutgoingTimeout(dnsConn.ReadTimeout, dnsConn.WriteTimeout)

							if err := writeMsgData(dnsConn.Net, dnsMsg.Data); err != nil {
								logger.Debug("dnsquery:writemsg",
									slog.String("host", q.Domain),
									slog.GroupAttrs("dns", dnsAttrs...),
									slog.Any("error", err))
								return err
							}

							dnsConn.Chrome.SetOutgoingTimeout(dnsConn.ReadTimeout, dnsConn.WriteTimeout)

							if _, err := dnsMsg.ReadFrom(dnsConn.Net); err != nil {
								logger.Debug("dnsquery:readmsg",
									slog.String("host", q.Domain),
									slog.GroupAttrs("dns", dnsAttrs...),
									slog.Any("error", err))
								return err
							}
						}

						dnsConn.Chrome.SetOutgoingTimeout(0, 0)

						if err := dnsMsg.Unpack(); err != nil {
							logger.Debug("dnsquery:unpackmsg",
								slog.String("host", q.Domain),
								slog.GroupAttrs("dns", dnsAttrs...),
								slog.Any("error", err))
							return err
						}

						var minTTL uint32 = math.MaxUint32

						for rr := range dnsMsg.RRs() {
							if h := rr.Header(); h != nil {
								if opts.DNS.TTL.Min > 0 || opts.DNS.TTL.Max > 0 {
									ttl := time.Duration(h.TTL) * time.Second
									if opts.DNS.TTL.Min > 0 && ttl < opts.DNS.TTL.Min {
										ttl = opts.DNS.TTL.Min
									}
									if opts.DNS.TTL.Max > 0 && ttl > opts.DNS.TTL.Max {
										ttl = opts.DNS.TTL.Max
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
									r.IPv4 = append(r.IPv4, rr.Addr)
								}
							case *dns.AAAA:
								if rr.Addr.IsValid() {
									r.IPv6 = append(r.IPv6, rr.Addr)
								}
							}
						}

						if minTTL < math.MaxUint32 {
							newDeadline := time.Now().Add(time.Duration(minTTL) * time.Second)
							if r.Deadline.IsZero() || newDeadline.Before(r.Deadline) {
								r.Deadline = newDeadline
							}
						}

						return nil
					}()
					if err != nil {
						closeConnection()
					} else {
						qtypes = qtypes[1:]
					}
				}

				if len(r.IPv4)+len(r.IPv6) != 0 {
					result = r

					if !r.Deadline.IsZero() {
						opts.dnsCache.Store(q.Domain, r)
					}

					logger.Debug("dnsquery",
						slog.String("host", q.Domain),
						slog.Any("ipv4", r.IPv4),
						slog.Any("ipv6", r.IPv6),
						slog.Duration("ttl", r.TTL()))
				}
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
	return x.DNS.TTL != y.DNS.TTL ||
		!reflect.DeepEqual(&x.DNS.Query, &y.DNS.Query) ||
		!reflect.DeepEqual(&x.DNS.Servers, &y.DNS.Servers)
}

func normalizeQueryTypes(qtypes []string) []string {
	if len(qtypes) == 0 {
		return qtypes
	}

	result := qtypes[:0]
Outer:
	for _, t := range qtypes {
		t = strings.ToUpper(t)
		if t != "A" && t != "AAAA" {
			continue
		}

		for _, r := range result {
			if r == t {
				continue Outer
			}
		}

		result = append(result, t)
	}

	return result
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

var (
	errNoAddress = errors.New("no address")
	errNoResult  = errors.New("no result")
)
