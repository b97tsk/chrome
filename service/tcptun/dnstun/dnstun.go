package dnstun

import (
	"context"
	"crypto/tls"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/b97tsk/chrome"
	"github.com/b97tsk/chrome/internal/proxy"
	"github.com/miekg/dns"
)

type Options struct {
	ListenAddr string `yaml:"on"`

	Server  DNServer
	Servers []DNServer

	Dial struct {
		Timeout time.Duration
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

	Proxy chrome.ProxyChain `yaml:"over"`

	dialer proxy.Dialer
}

type DNServer struct {
	Name string
	IP   chrome.StringList
	Over string
	Port uint16
}

type Service struct{}

const _ServiceName = "dnstun"

func (Service) Name() string {
	return _ServiceName
}

func (Service) Options() interface{} {
	return new(Options)
}

func (Service) Run(ctx chrome.Context) {
	logger := ctx.Manager.Logger(_ServiceName)

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

	var server net.Listener

	startServer := func() error {
		if server != nil {
			return nil
		}

		opts := <-optsOut

		ln, err := net.Listen("tcp", opts.ListenAddr)
		if err != nil {
			logger.Error(err)
			return err
		}

		defer logger.Infof("listening on %v", ln.Addr())

		server = ln

		dnsQueryIn := make(chan dnsQuery)

		go startWorker(ctx, dnsQueryIn)

		go ctx.Manager.Serve(ln, func(c net.Conn) {
			opts, ok := <-optsOut
			if !ok {
				return
			}

			local, localCtx := chrome.NewConnChecker(c)

			dnsConn := &dns.Conn{Conn: local}

			for {
				in, err := dnsConn.ReadMsg()
				if err != nil {
					logger.Tracef("(local) ReadMsg: %v", err)
					return
				}

				var result *dns.Msg

				r := make(chan *dns.Msg, 1)

				select {
				case <-ctx.Done():
					return
				case <-localCtx.Done():
					return
				case dnsQueryIn <- dnsQuery{in, r, localCtx, opts}:
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

				if err := dnsConn.WriteMsg(result); err != nil {
					logger.Tracef("(local) WriteMsg: %v", err)
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

MainLoop:
	for {
		select {
		case <-ctx.Done():
			return
		case opts := <-ctx.Load:
			if new, ok := opts.(*Options); ok {
				old := <-optsOut
				new := *new
				new.dialer = old.dialer

				if new.ListenAddr != old.ListenAddr {
					stopServer()
				}

				if len(new.Servers) == 0 {
					if new.Server.Name == "" && len(new.Server.IP) == 0 {
						logger.Error("DNS server is not specified")
						break
					}

					new.Servers = append(new.Servers, new.Server)
				}

				for i := range new.Servers {
					server := &new.Servers[i]
					if server.Name == "" && len(server.IP) == 0 {
						logger.Errorf("server #%v: invalid", i+1)
						continue MainLoop
					}

					server.Over = strings.ToUpper(server.Over)

					if server.Over == "TLS" && server.Name == "" {
						logger.Errorf("server #%v: DNS-over-TLS requires a server name", i+1)
						continue MainLoop
					}
				}

				if !new.Proxy.Equals(old.Proxy) {
					new.dialer = new.Proxy.NewDialer()
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
	Options Options
}

func startWorker(ctx chrome.Context, incoming <-chan dnsQuery) {
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

	var iplistBuffer []net.IP

	dnsConnIdleTimeout := defaultIdleTimeout
	dnsConnReadTimeout := defaultReadTimeout
	dnsConnWriteTimeout := defaultWriteTimeout

	logger := ctx.Manager.Logger(_ServiceName)

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

			logger.Trace("closing DNS connection due to idle timeout")

			dnsConn.Close()
			dnsConn = nil
		case q := <-incoming:
			opts := &q.Options

			var result *dns.Msg

			for {
				if ctx.Err() != nil {
					return
				}

				if q.Context.Err() != nil {
					break
				}

				if dnsConn == nil {
					server := opts.Servers[rand.Intn(len(opts.Servers))]

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
					logger.Tracef("dialing to %v", hostport)

					conn, err := ctx.Manager.Dial(q.Context, opts.dialer, "tcp", hostport, opts.Dial.Timeout)
					if err != nil {
						logger.Trace(err)
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
					_ = dnsConn.SetDeadline(time.Now().Add(dnsConnReadTimeout))

					result, err = dnsConn.ReadMsg()
					if err == nil {
						if len(result.Question) > 0 && logger.TraceWritable() {
							switch q0 := result.Question[0]; q0.Qtype {
							case dns.TypeA, dns.TypeAAAA:
								iplist := iplistBuffer[:0]

								for _, ans := range result.Answer {
									switch ans := ans.(type) {
									case *dns.A:
										iplist = append(iplist, ans.A)
									case *dns.AAAA:
										iplist = append(iplist, ans.AAAA)
									}
								}

								if len(iplist) > 0 {
									domain := strings.TrimSuffix(q0.Name, ".")
									logger.Tracef("(remote) %v: %v", domain, iplist)
								}

								iplistBuffer = iplist
							}
						}

						break
					}

					logger.Tracef("(remote) ReadMsg: %v", err)
				} else {
					logger.Tracef("(remote) WriteMsg: %v", err)
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
				case q.Result <- result:
				default:
				}
			}

			close(q.Result)
		}
	}
}

const (
	defaultIdleTimeout  = 10 * time.Second
	defaultReadTimeout  = 2 * time.Second
	defaultWriteTimeout = 3 * time.Second
)
