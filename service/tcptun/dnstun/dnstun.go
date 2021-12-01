package dnstun

import (
	"context"
	"crypto/tls"
	"io"
	"math"
	"math/rand"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/b97tsk/chrome"
	"github.com/b97tsk/chrome/internal/netutil"
	"github.com/miekg/dns"
)

type Options struct {
	ListenAddr string `yaml:"on"`

	Server  DNServer
	Servers []DNServer

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

	Proxy chrome.Proxy `yaml:"over"`

	dnsCache *sync.Map
}

type DNServer struct {
	Name string
	IP   chrome.StringList
	Over string
	Port uint16
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
							if r.Deadline.IsZero() || r.Deadline.After(time.Now()) {
								result = r.Message
								logger.Tracef("(from cache) %v: %v TTL=%v", domain, r.IPList, r.TTL())
							}
						}
					}
				}

				if result == nil {
					r := make(chan *dns.Msg, 1)

					select {
					case <-ctx.Done():
						return
					case <-localCtx.Done():
						return
					case dnsQueryIn <- dnsQuery{msg, r, localCtx}:
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
				new.dnsCache = old.dnsCache

				if _, _, err := net.SplitHostPort(new.ListenAddr); err != nil {
					logger.Error(err)
					return
				}

				if new.ListenAddr != old.ListenAddr {
					stopServer()
				}

				if len(new.Servers) == 0 {
					if new.Server.Name == "" && len(new.Server.IP) == 0 {
						logger.Error("DNS server is not specified")
						return
					}

					new.Servers = append(new.Servers, new.Server)
				}

				for i := range new.Servers {
					server := &new.Servers[i]
					if server.Name == "" && len(server.IP) == 0 {
						logger.Errorf("server #%v: invalid", i+1)
						return
					}

					server.Over = strings.ToUpper(server.Over)

					if server.Over == "TLS" && server.Name == "" {
						logger.Errorf("server #%v: DNS-over-TLS requires a server name", i+1)
						return
					}
				}

				if new.Cache != old.Cache {
					if new.Cache {
						if new.dnsCache == nil || shouldResetDNSCache(old, new) {
							new.dnsCache = &sync.Map{}
						}
					} else {
						new.dnsCache = nil
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

	logger := ctx.Manager.Logger(ServiceName)

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

			dnsConn.Close()
			dnsConn = nil
		case q := <-incoming:
			opts, ok := <-options
			if !ok {
				return
			}

			var result *dnsQueryResult

			var qtype uint16

			var domain string

			if len(q.Message.Question) != 0 {
				qtype = q.Message.Question[0].Qtype
				domain = strings.TrimSuffix(q.Message.Question[0].Name, ".")
			}

			if opts.dnsCache != nil && (qtype == dns.TypeA || qtype == dns.TypeAAAA) {
				if cache, ok := opts.dnsCache.Load(domain); ok {
					r := cache.(*dnsQueryResult)
					if r.Deadline.IsZero() || r.Deadline.After(time.Now()) {
						result = r
						logger.Tracef("(from cache) %v: %v TTL=%v", domain, r.IPList, r.TTL())
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

					server := opts.Servers[rand.Intn(len(opts.Servers))]

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

					conn, err := ctx.Manager.Dial(q.Context, opts.Proxy.Dialer(), "tcp", hostport, opts.Dial.Timeout)
					if err != nil {
						if err != context.Canceled {
							logger.Tracef("dial %v: %v", hostport, err)
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

								if ttl > 0 && r.Deadline.IsZero() {
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
								opts.dnsCache.Store(domain, &r)
							}

							logger.Debugf("(remote) %v: %v TTL=%v", domain, r.IPList, r.TTL())
						}

						break
					}

					logger.Tracef("(remote) read msg: %v", err)
				} else {
					logger.Tracef("(remote) write msg: %v", err)
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
	return x.TTL != y.TTL || !reflect.DeepEqual(&x.Servers, &y.Servers)
}

const (
	defaultIdleTimeout  = 10 * time.Second
	defaultReadTimeout  = 2 * time.Second
	defaultWriteTimeout = 3 * time.Second
)
