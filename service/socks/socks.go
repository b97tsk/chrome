package socks

import (
	"context"
	"crypto/tls"
	"math/rand"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/b97tsk/chrome/internal/proxy"
	"github.com/b97tsk/chrome/service"
	"github.com/miekg/dns"
	"github.com/shadowsocks/go-shadowsocks2/socks"
)

type Options struct {
	Proxy service.ProxyChain `yaml:"over"`

	Dial struct {
		Timeout time.Duration
	}

	DNS struct {
		Server  DNServer
		Servers []DNServer

		Query struct {
			Type service.StringList
			All  bool
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

		Proxy *service.ProxyChain `yaml:"over"`
	}

	dialer    proxy.Dialer
	dnsDialer proxy.Dialer
	dnsCache  *sync.Map
}

type DNServer struct {
	Name string
	IP   service.StringList
	Over string
	Port uint16
}

type Service struct{}

func (Service) Name() string {
	return "socks"
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

	defer ln.Close()

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

	var initialized bool

	initialize := func() {
		if initialized {
			return
		}

		initialized = true

		ctx.Manager.ServeListener(ln, func(c net.Conn) {
			opts, ok := <-optsOut
			if !ok {
				return
			}

			addr, err := socks.Handshake(c)
			if err != nil {
				ctx.Logger.Debugf("socks handshake: %v", err)
				return
			}

			local, localCtx := service.NewConnChecker(c)

			hostport := addr.String()

			if len(opts.DNS.Servers) > 0 {
				if host, port, _ := net.SplitHostPort(hostport); net.ParseIP(host) == nil {
					var result *dnsQueryResult

					if cache, ok := opts.dnsCache.Load(host); ok {
						r := cache.(*dnsQueryResult)
						if r.Deadline.IsZero() || r.Deadline.After(time.Now()) {
							result = r
							ctx.Logger.Tracef("[dns] (from cache) %v: %v TTL=%v", host, r.IPList, r.TTL)
						}
					}

					if result == nil {
						r := make(chan *dnsQueryResult, 1)

						select {
						case <-ctx.Done():
							return
						case <-localCtx.Done():
							return
						case dnsQueryIn <- dnsQuery{host, r, localCtx, opts}:
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

					if result != nil {
						ip := result.IPList[rand.Intn(len(result.IPList))]
						hostport = net.JoinHostPort(ip.String(), port)
					}
				}
			}

			remote, err := ctx.Manager.Dial(localCtx, opts.dialer, "tcp", hostport, opts.Dial.Timeout)
			if err != nil {
				ctx.Logger.Trace(err)
				return
			}
			defer remote.Close()

			service.Relay(local, remote)
		})
	}
MainLoop:
	for {
		select {
		case <-ctx.Done():
			return
		case opts := <-ctx.Opts:
			if new, ok := opts.(*Options); ok {
				old := <-optsOut
				new := *new
				new.dialer = old.dialer
				new.dnsDialer = old.dnsDialer
				new.dnsCache = old.dnsCache

				if !new.Proxy.Equals(old.Proxy) {
					new.dialer = new.Proxy.NewDialer()
				}

				if len(new.DNS.Servers) == 0 && (new.DNS.Server.Name != "" || len(new.DNS.Server.IP) > 0) {
					new.DNS.Servers = append(new.DNS.Servers, new.DNS.Server)
				}

				if len(new.DNS.Servers) > 0 {
					new.DNS.Query.Type = normalizeQueryTypes(new.DNS.Query.Type)
					if len(new.DNS.Query.Type) == 0 {
						new.DNS.Query.Type = service.StringList{"A"}
					}

					for i := range new.DNS.Servers {
						server := &new.DNS.Servers[i]
						if server.Name == "" && len(server.IP) == 0 {
							ctx.Logger.Errorf("[dns] server #%v: invalid", i+1)
							continue MainLoop
						}

						server.Over = strings.ToUpper(server.Over)

						if server.Over == "TLS" && server.Name == "" {
							ctx.Logger.Errorf("[dns] server #%v: DNS-over-TLS requires a server name", i+1)
							continue MainLoop
						}
					}

					switch {
					case new.DNS.Proxy == nil:
						new.dnsDialer = new.dialer
					case old.DNS.Proxy == nil || !new.DNS.Proxy.Equals(*old.DNS.Proxy):
						new.dnsDialer = new.DNS.Proxy.NewDialer()
					}

					if new.dnsCache == nil || shouldResetDNSCache(old, new) {
						new.dnsCache = &sync.Map{}
					}

					if dnsQueryIn == nil {
						dnsQueryIn = make(chan dnsQuery)

						go startWorker(ctx, dnsQueryIn)
					}
				}

				optsIn <- new

				initialize()
			}
		}
	}
}

type dnsQuery struct {
	Domain  string
	Result  chan<- *dnsQueryResult
	Context context.Context
	Options Options
}

type dnsQueryResult struct {
	IPList   []net.IP
	TTL      time.Duration
	Deadline time.Time
}

func startWorker(ctx service.Context, incoming <-chan dnsQuery) {
	var dnsConn *dns.Conn

	var dnsConnIdle struct {
		Timer  *time.Timer
		TimerC <-chan time.Time
	}

	defer func() {
		if dnsConnIdle.Timer != nil {
			dnsConnIdle.Timer.Stop()
		}

		if dnsConn != nil {
			dnsConn.Close()
		}
	}()

	var tlsConfig *tls.Config

	var tlsConfigCache map[string]*tls.Config

	dnsConnIdleTimeout := defaultIdleTimeout
	dnsConnReadTimeout := defaultReadTimeout
	dnsConnWriteTimeout := defaultWriteTimeout

	for {
		if dnsConn != nil {
			if dnsConnIdle.Timer == nil {
				dnsConnIdle.Timer = time.NewTimer(dnsConnIdleTimeout)
				dnsConnIdle.TimerC = dnsConnIdle.Timer.C
			} else {
				if !dnsConnIdle.Timer.Stop() {
					<-dnsConnIdle.TimerC
				}
				dnsConnIdle.Timer.Reset(dnsConnIdleTimeout)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-dnsConnIdle.TimerC:
			dnsConnIdle.Timer = nil
			dnsConnIdle.TimerC = nil

			ctx.Logger.Trace("[dns] closing DNS connection due to idle timeout")

			dnsConn.Close()
			dnsConn = nil
		case q := <-incoming:
			opts := &q.Options

			var result *dnsQueryResult

			if cache, ok := opts.dnsCache.Load(q.Domain); ok {
				r := cache.(*dnsQueryResult)
				if r.Deadline.IsZero() || r.Deadline.After(time.Now()) {
					result = r
					ctx.Logger.Tracef("[dns] (from cache) %v: %v TTL=%v", q.Domain, r.IPList, r.TTL)
				}
			}

			if result == nil {
				var r dnsQueryResult

				fqDomain := dns.Fqdn(q.Domain)

				qtypes := opts.DNS.Query.Type
				for len(qtypes) > 0 {
					if ctx.Err() != nil {
						return
					}

					if q.Context.Err() != nil {
						break
					}

					if dnsConn == nil {
						server := opts.DNS.Servers[rand.Intn(len(opts.DNS.Servers))]

						host := server.Name

						if len(server.IP) > 0 {
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
						ctx.Logger.Tracef("[dns] dialing to %v", hostport)

						conn, err := ctx.Manager.Dial(q.Context, opts.dnsDialer, "tcp", hostport, opts.Dial.Timeout)
						if err != nil {
							ctx.Logger.Tracef("[dns] %v", err)
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

						if opts.DNS.Idle.Timeout > 0 {
							dnsConnIdleTimeout = opts.DNS.Idle.Timeout
						}

						if opts.DNS.Read.Timeout > 0 {
							dnsConnReadTimeout = opts.DNS.Read.Timeout
						}

						if opts.DNS.Write.Timeout > 0 {
							dnsConnWriteTimeout = opts.DNS.Write.Timeout
						}
					}

					var m dns.Msg

					switch qtypes[0] {
					case "A":
						m.SetQuestion(fqDomain, dns.TypeA)
					case "AAAA":
						m.SetQuestion(fqDomain, dns.TypeAAAA)
					}

					_ = dnsConn.SetDeadline(time.Now().Add(dnsConnWriteTimeout))

					err := dnsConn.WriteMsg(&m)
					if err == nil {
						var in *dns.Msg

						_ = dnsConn.SetDeadline(time.Now().Add(dnsConnReadTimeout))

						in, err = dnsConn.ReadMsg()
						if err == nil {
							for _, ans := range in.Answer {
								if r.Deadline.IsZero() {
									if h := ans.Header(); h != nil {
										ttl := time.Duration(h.Ttl) * time.Second
										if opts.DNS.TTL.Min > 0 && ttl < opts.DNS.TTL.Min {
											ttl = opts.DNS.TTL.Min
										}

										if opts.DNS.TTL.Max > 0 && ttl > opts.DNS.TTL.Max {
											ttl = opts.DNS.TTL.Max
										}

										if ttl > 0 {
											r.TTL = ttl
											r.Deadline = time.Now().Add(ttl)
										}
									}
								}

								switch ans := ans.(type) {
								case *dns.A:
									r.IPList = append(r.IPList, ans.A)
								case *dns.AAAA:
									r.IPList = append(r.IPList, ans.AAAA)
								}
							}

							qtypes = qtypes[1:]

							if !opts.DNS.Query.All && len(r.IPList) > 0 {
								break
							}
						} else {
							ctx.Logger.Tracef("[dns] ReadMsg: %v", err)
						}
					} else {
						ctx.Logger.Tracef("[dns] WriteMsg: %v", err)
					}

					if err != nil {
						if dnsConnIdle.Timer != nil {
							dnsConnIdle.Timer.Stop()
							dnsConnIdle.Timer = nil
							dnsConnIdle.TimerC = nil
						}

						dnsConn.Close()
						dnsConn = nil
					}
				}

				if len(r.IPList) > 0 {
					result = &r
					opts.dnsCache.Store(q.Domain, &r)
					ctx.Logger.Debugf("[dns] %v: %v TTL=%v", q.Domain, r.IPList, r.TTL)
				}
			}

			if result != nil {
				select {
				case <-ctx.Done():
					return
				case <-q.Context.Done():
				case q.Result <- result:
				default:
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

const (
	defaultIdleTimeout  = 10 * time.Second
	defaultReadTimeout  = 2 * time.Second
	defaultWriteTimeout = 3 * time.Second
)
