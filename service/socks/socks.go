package socks

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"math"
	"math/rand"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/b97tsk/chrome"
	"github.com/b97tsk/chrome/internal/ioutil"
	"github.com/b97tsk/proxy"
	"github.com/miekg/dns"
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
	Name string
	IP   chrome.StringList
	Over string
	Port uint16
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

		go ctx.Manager.Serve(ln, func(local net.Conn) {
			var reply bytes.Buffer

			rw := &struct {
				io.Reader
				io.Writer
			}{local, ioutil.LimitWriter(local, 2, &reply)}

			addr, err := socks.Handshake(rw)
			if err != nil {
				return
			}

			opts, ok := <-optsOut
			if !ok {
				return
			}

			if _, err := reply.WriteTo(local); err != nil {
				logger.Tracef("write response to local: %v", err)
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
								logger.Tracef("[dns] (from cache) %v: %v %v TTL=%v", host, r.IPv4, r.IPv6, r.TTL())
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
									ip := ips[rand.Intn(len(ips))]
									return net.JoinHostPort(ip.String(), port)
								}
							}

							if ips := result.IPv6; len(ips) != 0 {
								getaddr2 = func() string {
									ip := ips[rand.Intn(len(ips))]
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
				new.dnsCache = old.dnsCache

				if _, _, err := net.SplitHostPort(new.ListenAddr); err != nil {
					logger.Error(err)
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
						if server.Name == "" && len(server.IP) == 0 {
							logger.Errorf("[dns] server #%v: invalid", i+1)
							return
						}

						server.Over = strings.ToUpper(server.Over)

						if server.Over == "TLS" && server.Name == "" {
							logger.Errorf("[dns] server #%v: DNS-over-TLS requires a server name", i+1)
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
	IPv4, IPv6 []net.IP
	Deadline   time.Time
}

func (r *dnsQueryResult) TTL() time.Duration {
	return time.Until(r.Deadline).Truncate(time.Second)
}

func startWorker(ctx chrome.Context, options <-chan Options, incoming <-chan dnsQuery) {
	var dnsConn struct {
		*dns.Conn
		sync.Mutex
		sync.WaitGroup
		Timeout     time.Duration
		Deadline    time.Time
		GotResponse bool
		IdleTimer   *time.Timer
		IdleTimerC  <-chan time.Time
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
		if dnsConn.IdleTimer != nil {
			dnsConn.IdleTimer.Stop()
			dnsConn.IdleTimer = nil
			dnsConn.IdleTimerC = nil
		}
	}

	defer func() {
		if dnsConn.Conn != nil {
			closeConnection()
		}
	}()

	dnsConnIdleTimeout := defaultIdleTimeout
	dnsConnReadTimeout := defaultReadTimeout
	dnsConnWriteTimeout := defaultWriteTimeout

	logger := ctx.Manager.Logger(ctx.JobName)

	for {
		if dnsConn.Conn != nil {
			if dnsConn.IdleTimer == nil {
				dnsConn.IdleTimer = time.NewTimer(dnsConnIdleTimeout)
				dnsConn.IdleTimerC = dnsConn.IdleTimer.C
			} else {
				if !dnsConn.IdleTimer.Stop() {
					<-dnsConn.IdleTimerC
				}
				dnsConn.IdleTimer.Reset(dnsConnIdleTimeout)
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
					logger.Tracef("[dns] (from cache) %v: %v %v TTL=%v", q.Domain, r.IPv4, r.IPv6, r.TTL())
				}
			}

			if result == nil {
				r := &dnsQueryResult{}

				fqDomain := dns.Fqdn(q.Domain)

				qtypes := opts.DNS.Query.Type
				for len(qtypes) != 0 {
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

						server := opts.DNS.Servers[rand.Intn(len(opts.DNS.Servers))]

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

						if opts.DNS.Idle.Timeout > 0 {
							dnsConnIdleTimeout = opts.DNS.Idle.Timeout
						}

						if opts.DNS.Read.Timeout > 0 {
							dnsConnReadTimeout = opts.DNS.Read.Timeout
						}

						if opts.DNS.Write.Timeout > 0 {
							dnsConnWriteTimeout = opts.DNS.Write.Timeout
						}

						dnsConn.Unlock()
					}

					var m dns.Msg

					switch qtypes[0] {
					case "A":
						m.SetQuestion(fqDomain, dns.TypeA)
					case "AAAA":
						m.SetQuestion(fqDomain, dns.TypeAAAA)
					}

					setTimeout(dnsConnWriteTimeout)

					err := dnsConn.WriteMsg(&m)
					if err == nil {
						var msg *dns.Msg

						setTimeout(dnsConnReadTimeout)

						msg, err = dnsConn.ReadMsg()
						if err == nil {
							var minTTL uint32 = math.MaxUint32

							for _, ans := range msg.Answer {
								if h := ans.Header(); h != nil {
									if opts.DNS.TTL.Min > 0 || opts.DNS.TTL.Max > 0 {
										ttl := time.Duration(h.Ttl) * time.Second

										if opts.DNS.TTL.Min > 0 && ttl < opts.DNS.TTL.Min {
											ttl = opts.DNS.TTL.Min
										}

										if opts.DNS.TTL.Max > 0 && ttl > opts.DNS.TTL.Max {
											ttl = opts.DNS.TTL.Max
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
									r.IPv4 = append(r.IPv4, ans.A)
								case *dns.AAAA:
									r.IPv6 = append(r.IPv6, ans.AAAA)
								}
							}

							if minTTL < math.MaxUint32 {
								newDeadline := time.Now().Add(time.Duration(minTTL) * time.Second)
								if r.Deadline.IsZero() || newDeadline.Before(r.Deadline) {
									r.Deadline = newDeadline
								}
							}

							qtypes = qtypes[1:]
						} else {
							logger.Tracef("[dns] read msg: %v", err)
						}
					} else {
						logger.Tracef("[dns] write msg: %v", err)
					}

					if err != nil {
						closeConnection()
					}
				}

				if len(r.IPv4)+len(r.IPv6) != 0 {
					result = r

					if !r.Deadline.IsZero() {
						opts.dnsCache.Store(q.Domain, r)
					}

					logger.Debugf("[dns] %v: %v %v TTL=%v", q.Domain, r.IPv4, r.IPv6, r.TTL())
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

const (
	defaultIdleTimeout  = 10 * time.Second
	defaultReadTimeout  = 2 * time.Second
	defaultWriteTimeout = 3 * time.Second
)

var (
	errNoAddress = errors.New("no address")
	errNoResult  = errors.New("no result")
)
