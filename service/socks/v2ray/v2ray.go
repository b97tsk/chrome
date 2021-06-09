package v2ray

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/b97tsk/chrome"
	"github.com/b97tsk/chrome/internal/ioutil"
	"github.com/b97tsk/chrome/internal/netutil"
	"github.com/shadowsocks/go-shadowsocks2/socks"
)

type Options struct {
	ListenAddr string `yaml:"on"`

	URL string

	Type      string
	Protocol  string `yaml:"-"`
	Transport string `yaml:"-"`

	ProtocolOptions  `yaml:",inline"`
	TransportOptions `yaml:",inline"`

	Mux struct {
		Enabled       bool        `json:"enabled" yaml:"-"`
		EnabledByYAML *bool       `json:"-" yaml:"enabled"`
		Concurrency   int         `json:"concurrency,omitempty"`
		Ping          PingOptions `json:"-"`
	}

	Policy struct {
		Handshake    int `json:"handshake,omitempty"`
		ConnIdle     int `json:"connIdle,omitempty"`
		UplinkOnly   int `json:"uplinkOnly"`
		DownlinkOnly int `json:"downlinkOnly"`
	}

	Proxy chrome.Proxy `yaml:"over"`

	ForwardServer HostportOptions `yaml:"-"`

	Dial struct {
		Timeout time.Duration
	}
	Relay chrome.RelayOptions

	stats statsINFO
}

type statsINFO struct {
	ins        *instance
	ctx        context.Context
	cancel     context.CancelFunc
	readevents chan struct{}
}

type ProtocolOptions struct {
	FREEDOM struct {
		DomainStrategy string `json:"domainStrategy,omitempty"`
		Redirect       string `json:"redirect,omitempty"`
	}
	TROJAN struct {
		HostportOptions `yaml:",inline"`
		Password        string
	}
	VMESS struct {
		HostportOptions `yaml:",inline"`
		ID              string
		AlterID         int `yaml:"aid"`
	}
}

type TransportOptions struct {
	HTTP struct {
		Host []string
		Path string
	}
	KCP struct {
		Header string
	}
	TCP struct{}
	TLS struct {
		Enabled    bool   `json:"-" yaml:"-"`
		ServerName string `json:"serverName"`
	}
	WS struct {
		Path   string
		Header map[string]string
	}
}

type HostportOptions struct {
	Address string `yaml:"hostport"`
	Port    int    `yaml:"-"`
}

type PingOptions struct {
	Enabled  bool
	URL      string
	URLs     []string
	Number   int
	Timeout  time.Duration
	Interval struct {
		Start time.Duration
		Step  time.Duration
		Max   time.Duration
	}
}

type Service struct{}

const _ServiceName = "v2ray"

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

		go ctx.Manager.Serve(ln, func(c net.Conn) {
			opts := <-optsOut
			if opts.stats.ins == nil {
				return
			}

			var reply bytes.Buffer

			rw := &struct {
				io.Reader
				io.Writer
			}{c, ioutil.LimitWriter(c, 2, &reply)}

			addr, err := socks.Handshake(rw)
			if err != nil {
				logger.Debugf("socks handshake: %v", err)
				return
			}

			local, localCtx := chrome.NewConnChecker(c)

			remote, err := ctx.Manager.Dial(localCtx, opts.stats.ins, "tcp", addr.String(), opts.Dial.Timeout)
			if err != nil {
				logger.Trace(err)
				return
			}
			defer remote.Close()

			if _, err := reply.WriteTo(local); err != nil {
				logger.Trace(err)
				return
			}

			readevents := opts.stats.readevents
			remote = netutil.DoR(remote, func(int) {
				select {
				case readevents <- struct{}{}:
				default:
				}
			})

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

	var (
		stats      statsINFO
		restart    chan struct{}
		cancelPing context.CancelFunc
	)

	stopInstance := func() {
		if cancelPing != nil {
			cancelPing()
			cancelPing = nil
		}

		if stats.ins != nil {
			go func(stats statsINFO) {
				defer func() {
					if err := stats.ins.Close(); err != nil {
						logger.Debugf("close instance: %v", err)
					}

					stats.cancel()
				}()

				var recentReadTime time.Time

				t := time.NewTimer(remoteIdleTime)

				for {
					select {
					case <-t.C:
						d := time.Until(recentReadTime.Add(remoteIdleTime))
						if d <= 0 {
							return
						}

						t.Reset(d)
					case <-stats.readevents:
						recentReadTime = time.Now()
					}
				}
			}(stats)

			stats = statsINFO{}
		}
	}
	defer stopInstance()

	startInstance := func(opts Options) {
		ins, err := createInstance(opts)
		if err != nil {
			logger.Errorf("create instance: %v", err)
			return
		}

		if err := ins.Start(); err != nil {
			logger.Errorf("start instance: %v", err)
			return
		}

		stats.ins = ins
		stats.ctx, stats.cancel = context.WithCancel(context.Background())
		stats.readevents = make(chan struct{})

		if opts.Mux.Enabled && opts.Mux.Ping.Enabled {
			var ctxPing context.Context
			ctxPing, cancelPing = context.WithCancel(context.Background())

			if restart == nil {
				restart = make(chan struct{})
			}

			go func() {
				err := startPing(ctxPing, opts.Mux.Ping, opts.ListenAddr, restart)
				if err != nil {
					logger.Errorf("ping: %v", err)
				}
			}()
		}
	}

	var forwardListener net.Listener
	defer func() {
		if forwardListener != nil {
			_ = forwardListener.Close()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case opts := <-ctx.Load:
			if new, ok := opts.(*Options); ok {
				old := <-optsOut
				new := *new
				new.stats = old.stats

				if new.ListenAddr != old.ListenAddr {
					stopServer()
				}

				if !new.Proxy.IsZero() {
					if forwardListener == nil {
						ln, err := net.Listen("tcp", "localhost:")
						if err != nil {
							logger.Errorf("start forward server: %v", err)
							return
						}

						forwardListener = ln

						go ctx.Manager.Serve(ln, func(c net.Conn) {
							opts, ok := <-optsOut
							if !ok {
								return
							}

							var reply bytes.Buffer

							rw := &struct {
								io.Reader
								io.Writer
							}{c, ioutil.LimitWriter(c, 2, &reply)}

							addr, err := socks.Handshake(rw)
							if err != nil {
								logger.Debugf("socks handshake: %v", err)
								return
							}

							local, localCtx := chrome.NewConnChecker(c)

							remote, err := ctx.Manager.Dial(
								localCtx,
								opts.Proxy.Dialer(),
								"tcp",
								addr.String(),
								opts.Dial.Timeout,
							)
							if err != nil {
								logger.Trace(err)
								return
							}
							defer remote.Close()

							if _, err := reply.WriteTo(local); err != nil {
								logger.Trace(err)
								return
							}

							ctx.Manager.Relay(local, remote, opts.Relay)
						})
					}

					host, port, _ := net.SplitHostPort(forwardListener.Addr().String())
					new.ForwardServer.Address = host
					new.ForwardServer.Port, _ = strconv.Atoi(port)
				}

				if shouldRestart(old, new) {
					stopInstance()
					startInstance(new)
					new.stats = stats
				}

				optsIn <- new
			}
		case <-ctx.Loaded:
			if err := startServer(); err != nil {
				return
			}
		case <-restart:
			opts := <-optsOut

			stopInstance()
			startInstance(opts)

			opts.stats = stats
			optsIn <- opts
		}
	}
}

func shouldRestart(x, y Options) bool {
	if x.Proxy.IsZero() != y.Proxy.IsZero() {
		return true
	}

	var z Options
	x.Proxy, y.Proxy = z.Proxy, z.Proxy
	x.Dial, y.Dial = z.Dial, z.Dial
	x.Relay, y.Relay = z.Relay, z.Relay
	x.stats, y.stats = z.stats, z.stats

	return !reflect.DeepEqual(x, y)
}

func createInstance(opts Options) (*instance, error) {
	if err := parseURL(&opts); err != nil {
		return nil, err
	}

	opts.Protocol = "FREEDOM"
	opts.Transport = "TCP"

	for _, t := range strings.SplitN(opts.Type, "+", 3) {
		t = strings.ToUpper(t)
		switch t {
		case "FREEDOM", "TROJAN", "VMESS":
			opts.Protocol = t
		case "HTTP", "KCP", "TCP", "WS":
			opts.Transport = t
		case "TLS":
			opts.TLS.Enabled = true
		default:
			return nil, errors.New("unknown type: " + opts.Type)
		}
	}

	var hostport *HostportOptions

	switch opts.Protocol {
	case "TROJAN":
		hostport = &opts.TROJAN.HostportOptions
	case "VMESS":
		hostport = &opts.VMESS.HostportOptions
	}

	if hostport != nil {
		host, port, err := net.SplitHostPort(hostport.Address)
		if err != nil {
			return nil, errors.New("invalid address: " + hostport.Address)
		}

		hostport.Address = host

		hostport.Port, err = strconv.Atoi(port)
		if err != nil {
			return nil, errors.New("invalid port in address: " + hostport.Address)
		}
	}

	if opts.Transport == "KCP" {
		if opts.KCP.Header == "" {
			opts.KCP.Header = "none"
		}
	}

	if opts.Mux.EnabledByYAML != nil {
		opts.Mux.Enabled = *opts.Mux.EnabledByYAML
	} else {
		switch opts.Protocol {
		case "TROJAN", "VMESS":
			opts.Mux.Enabled = true
		}
	}

	var buf bytes.Buffer

	if err := v2rayTemplate.Execute(&buf, &opts); err != nil {
		return nil, err
	}

	return newInstanceFromJSON(buf.Bytes())
}

func parseURL(opts *Options) error {
	if opts.URL == "" {
		return nil
	}

	switch {
	case strings.HasPrefix(opts.URL, "trojan://"):
		return parseTrojanURL(opts)
	case strings.HasPrefix(opts.URL, "vmess://"):
		return parseVMessURL(opts)
	}

	return fmt.Errorf("unknown scheme in url %v", opts.URL)
}

func parseTrojanURL(opts *Options) error {
	u, err := url.Parse(opts.URL)
	if err != nil {
		return fmt.Errorf("parse trojan url %v: %w", opts.URL, err)
	}

	if u.User == nil {
		return fmt.Errorf("invalid trojan url %v", opts.URL)
	}

	opts.Type = "TROJAN+TCP+TLS"
	opts.TROJAN.Address = u.Host
	opts.TROJAN.Password = u.User.Username()

	return nil
}

func parseVMessURL(opts *Options) error {
	b64 := strings.TrimPrefix(opts.URL, "vmess://")

	b64 = strings.ReplaceAll(b64, "-", "+")
	b64 = strings.ReplaceAll(b64, "_", "/")

	enc := base64.StdEncoding
	if len(b64)%4 != 0 {
		enc = base64.RawStdEncoding
	}

	data, err := enc.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("decode vmess url %v: %w", opts.URL, err)
	}

	var config struct {
		Version json.RawMessage `json:"v"`

		Net  string `json:"net"`
		TLS  string `json:"tls"`
		Type string `json:"type"`

		Address string          `json:"add"`
		Port    json.RawMessage `json:"port"`
		ID      string          `json:"id"`
		AlterID json.RawMessage `json:"aid"`

		Path string `json:"path"`
		Host string `json:"host"`
	}

	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("unmarshal decoded vmess url %v: %w", opts.URL, err)
	}

	if unquote(string(config.Version)) == "" {
		fields := strings.SplitN(config.Path, ";", 2)
		if len(fields) == 2 {
			config.Path, config.Host = fields[0], fields[1]
		}
	}

	var transport string

	switch strings.ToUpper(config.Net) {
	case "HTTP", "H2":
		transport = "HTTP"

		if config.Host != "" {
			opts.HTTP.Host = []string{config.Host}
			if config.Host != config.Address {
				opts.TLS.ServerName = config.Host
			}
		}

		opts.HTTP.Path = config.Path
	case "KCP":
		transport = "KCP"
		opts.KCP.Header = config.Type
	case "TCP":
		transport = "TCP"

		if config.Type != "" && config.Type != "none" {
			return fmt.Errorf("unknown type field in vmess url %v: %v", opts.URL, config.Type)
		}

		if strings.EqualFold(config.TLS, "TLS") {
			transport = "TCP+TLS"

			if config.Host != "" && config.Host != config.Address {
				opts.TLS.ServerName = config.Host
			}
		}
	case "WS":
		transport = "WS"
		if strings.EqualFold(config.TLS, "TLS") {
			transport = "WS+TLS"

			if config.Host != "" && config.Host != config.Address {
				opts.TLS.ServerName = config.Host
			}
		}

		opts.WS.Path = config.Path
	default:
		return fmt.Errorf("unknown net field in vmess url %v: %v", opts.URL, config.Net)
	}

	opts.Type = "VMESS+" + transport
	opts.VMESS.Address = net.JoinHostPort(config.Address, unquote(string(config.Port)))
	opts.VMESS.ID = config.ID
	opts.VMESS.AlterID, _ = strconv.Atoi(unquote(string(config.AlterID)))

	return nil
}

func startPing(ctx context.Context, opts PingOptions, laddr string, restart chan<- struct{}) error {
	urls := opts.URLs
	if len(urls) == 0 {
		url := defaultPingURL
		if opts.URL != "" {
			url = opts.URL
		}

		urls = []string{url}
	}

	if opts.Number < 1 {
		opts.Number = defaultPingNumber
	}

	if opts.Timeout < 1 {
		opts.Timeout = defaultPingTimeout
	}

	if opts.Interval.Start < 1 {
		opts.Interval.Start = defaultPingIntervalStart
	}

	if opts.Interval.Step < 1 {
		opts.Interval.Step = defaultPingIntervalStep
	}

	if opts.Interval.Max < 1 {
		opts.Interval.Max = defaultPingIntervalMax
	}

	jar, _ := cookiejar.New(nil)

	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(&url.URL{Scheme: "socks5", Host: laddr}),
		},
		Jar: jar,
	}
	defer client.CloseIdleConnections()

	done := ctx.Done()
	number := opts.Number
	sleep := time.Duration(0)

	for {
		ctx, cancel := context.WithTimeout(ctx, opts.Timeout)

		url := urls[0]
		if len(urls) > 1 {
			url = urls[rand.Intn(len(urls))]
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			cancel()
			return err
		}

		req.Header.Set("User-Agent", "") // Reduce header size.

		resp, err := client.Do(req)
		if err == nil {
			const maxBodySlurpSize = 2 << 10
			if resp.ContentLength <= maxBodySlurpSize {
				_, _ = io.CopyN(io.Discard, resp.Body, maxBodySlurpSize)
			}

			resp.Body.Close()
		}

		if err != nil {
			number--
			sleep = 0 //nolint:wsl
		} else {
			number = opts.Number
			switch {
			case sleep == 0:
				sleep = opts.Interval.Start
			case sleep < opts.Interval.Max:
				sleep += opts.Interval.Step
			}
		}

		cancel()

		if number < 1 {
			select {
			case <-done:
			case restart <- struct{}{}:
			}

			return nil
		}

		sleep := sleep
		if sleep < 1 {
			sleep = opts.Interval.Start
		}

		select {
		case <-done:
			return nil
		case <-time.After(sleep):
		}
	}
}

func unquote(s string) string {
	if strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
		return s[1 : len(s)-1]
	}

	return s
}

const remoteIdleTime = 60 * time.Second

const (
	defaultPingURL           = "http://www.google.com/gen_204"
	defaultPingNumber        = 4
	defaultPingTimeout       = 5 * time.Second
	defaultPingIntervalStart = 1 * time.Second
	defaultPingIntervalStep  = 2 * time.Second
	defaultPingIntervalMax   = 11 * time.Second
)
