package socks

import (
	"bytes"
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
	"github.com/b97tsk/chrome/internal/ioutil"
	"github.com/miekg/dns"
	"github.com/shadowsocks/go-shadowsocks2/socks"
)

type Options struct {
	ListenAddr string `yaml:"on"`

	Proxy chrome.Proxy `yaml:"over"`

	Dial  chrome.DialOptions
	Relay chrome.RelayOptions

	DNS struct {
		Server  DNServer
		Servers []DNServer

		Query struct {
			Type chrome.StringList
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
			var reply bytes.Buffer

			rw := &struct {
				io.Reader
				io.Writer
			}{c, ioutil.LimitWriter(c, 2, &reply)}

			addr, err := socks.Handshake(rw)
			if err != nil {
				return
			}

			remoteAddr := addr.String()

			getopts := func() (chrome.RelayOptions, bool) {
				opts, ok := <-optsOut
				return opts.Relay, ok
			}

			getRemote := func(localCtx context.Context) net.Conn {
				opts, ok := <-optsOut
				if !ok {
					return nil
				}

				if len(opts.DNS.Servers) != 0 {
					if host, port, _ := net.SplitHostPort(remoteAddr); net.ParseIP(host) == nil {
						var result *dnsQueryResult

						if cache, ok := opts.dnsCache.Load(host); ok {
							r := cache.(*dnsQueryResult)
							if r.Deadline.After(time.Now()) {
								result = r
								logger.Tracef("[dns] (from cache) %v: %v TTL=%v", host, r.IPList, r.TTL())
							}
						}

						if result == nil {
							r := make(chan *dnsQueryResult, 1)

							select {
							case <-ctx.Done():
								return nil
							case <-localCtx.Done():
								return nil
							case dnsQueryIn <- dnsQuery{host, r, localCtx}:
							}

							select {
							case <-ctx.Done():
								return nil
							case <-localCtx.Done():
								return nil
							case result = <-r:
							}

							if result == nil {
								return nil
							}
						}

						if result != nil {
							ip := result.IPList[rand.Intn(len(result.IPList))]
							remoteAddr = net.JoinHostPort(ip.String(), port)
						}
					}
				}

				getopts := func() (chrome.Proxy, chrome.DialOptions, bool) {
					opts, ok := <-optsOut
					return opts.Proxy, opts.Dial, ok
				}

				remote, _ := ctx.Manager.Dial(localCtx, "tcp", remoteAddr, getopts, logger)

				return remote
			}

			sendResponse := func(w io.Writer) bool {
				if _, err := reply.WriteTo(w); err != nil {
					logger.Tracef("write response to local: %v", err)
					return false
				}

				return true
			}

			ctx.Manager.Relay(c, getopts, getRemote, sendResponse, logger)
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
						new.DNS.Query.Type = chrome.StringList{"A"}
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

			if cache, ok := opts.dnsCache.Load(q.Domain); ok {
				r := cache.(*dnsQueryResult)
				if r.Deadline.After(time.Now()) {
					result = r
					logger.Tracef("[dns] (from cache) %v: %v TTL=%v", q.Domain, r.IPList, r.TTL())
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

					if dnsConn == nil {
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

						getopts := func() (chrome.Proxy, chrome.DialOptions, bool) {
							opts, ok := <-options

							if opts.DNS.Proxy != nil {
								opts.Proxy = *opts.DNS.Proxy
							}

							return opts.Proxy, opts.Dial, ok
						}

						conn, err := ctx.Manager.Dial(q.Context, "tcp", hostport, getopts, logger)
						if err != nil {
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

					deadline := time.Now().Add(dnsConnWriteTimeout)
					_ = dnsConn.SetDeadline(deadline)

					err := dnsConn.WriteMsg(&m)
					if err == nil {
						var msg *dns.Msg

						deadline = time.Now().Add(dnsConnReadTimeout)
						_ = dnsConn.SetDeadline(deadline)

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
									r.IPList = append(r.IPList, ans.A)
								case *dns.AAAA:
									r.IPList = append(r.IPList, ans.AAAA)
								}
							}

							if minTTL < math.MaxUint32 {
								newDeadline := time.Now().Add(time.Duration(minTTL) * time.Second)
								if r.Deadline.IsZero() || newDeadline.Before(r.Deadline) {
									r.Deadline = newDeadline
								}
							}

							qtypes = qtypes[1:]

							if !opts.DNS.Query.All && len(r.IPList) != 0 {
								break
							}
						} else {
							logger.Tracef("[dns] read msg: %v", err)
						}
					} else {
						logger.Tracef("[dns] write msg: %v", err)
					}

					if err != nil {
						if dnsConnIdle.Timer != nil {
							dnsConnIdle.Timer.Stop()
							dnsConnIdle.Timer = nil
							dnsConnIdle.TimerC = nil
						}

						dnsConn.Close()
						dnsConn = nil

						if d := time.Until(deadline); d > 0 {
							if max := 2 * time.Second; d > max {
								d = max
							}

							select {
							case <-time.After(d):
							case <-ctx.Done():
							case <-q.Context.Done():
							}
						}
					}
				}

				if len(r.IPList) != 0 {
					result = r

					if !r.Deadline.IsZero() {
						opts.dnsCache.Store(q.Domain, r)
					}

					logger.Debugf("[dns] %v: %v TTL=%v", q.Domain, r.IPList, r.TTL())
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
