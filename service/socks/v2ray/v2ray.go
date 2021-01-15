package v2ray

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/b97tsk/chrome/service"
	"github.com/shadowsocks/go-shadowsocks2/socks"
	"gopkg.in/yaml.v2"
)

type Options struct {
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

	Dial struct {
		Timeout time.Duration
	}

	stats statsINFO
}

type statsINFO struct {
	ins        *Instance
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

func (Service) Name() string {
	return "v2ray"
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

	var initialized bool

	initialize := func() {
		if initialized {
			return
		}

		initialized = true

		ctx.Manager.ServeListener(ln, func(c net.Conn) {
			opts, ok := <-optsOut
			if !ok || opts.stats.ins == nil {
				return
			}

			addr, err := socks.Handshake(c)
			if err != nil {
				ctx.Logger.Debugf("socks handshake: %v", err)
				return
			}

			local, localCtx := service.NewConnCheckerContext(opts.stats.ctx, c)

			remote, err := ctx.Manager.Dial(localCtx, opts.stats.ins, "tcp", addr.String(), opts.Dial.Timeout)
			if err != nil {
				ctx.Logger.Trace(err)
				return
			}
			defer remote.Close()

			readevents := opts.stats.readevents
			remote = doOnRead(remote, func(int) {
				select {
				case readevents <- struct{}{}:
				default:
				}
			})

			service.Relay(local, remote)
		})
	}

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
						ctx.Logger.Debugf("close instance: %v", err)
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
		i, err := createInstance(opts)
		if err != nil {
			ctx.Logger.Errorf("create instance: %v", err)
			return
		}

		if err := i.Start(); err != nil {
			ctx.Logger.Errorf("start instance: %v", err)
			return
		}

		stats.ins = i
		stats.ctx, stats.cancel = context.WithCancel(context.Background())
		stats.readevents = make(chan struct{})

		if opts.Mux.Enabled && opts.Mux.Ping.Enabled {
			var ctxPing context.Context
			ctxPing, cancelPing = context.WithCancel(context.Background())

			if restart == nil {
				restart = make(chan struct{})
			}

			go func() {
				err := startPing(ctxPing, opts.Mux.Ping, ctx.ListenAddr, restart)
				if err != nil {
					ctx.Logger.Errorf("ping: %v", err)
				}
			}()
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case opts := <-ctx.Opts:
			if new, ok := opts.(Options); ok {
				old := <-optsOut
				new.stats = old.stats

				if shouldRestart(old, new) {
					stopInstance()
					startInstance(new)
					new.stats = stats
				}

				optsIn <- new

				initialize()
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

func (Service) UnmarshalOptions(text []byte) (interface{}, error) {
	var opts Options
	if err := yaml.UnmarshalStrict(text, &opts); err != nil {
		return nil, err
	}

	return opts, nil
}

type doOnReadConn struct {
	net.Conn
	do func(int)
}

func doOnRead(c net.Conn, do func(int)) net.Conn {
	return &doOnReadConn{c, do}
}

func (c *doOnReadConn) Read(p []byte) (n int, err error) {
	n, err = c.Conn.Read(p)
	if n > 0 {
		c.do(n)
	}

	return
}

func shouldRestart(x, y Options) bool {
	var z Options
	x.Dial, y.Dial = z.Dial, z.Dial
	x.stats, y.stats = z.stats, z.stats

	return !reflect.DeepEqual(x, y)
}

func createInstance(opts Options) (*Instance, error) {
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

	return NewInstanceFromJSON(buf.Bytes())
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

	reqURL := urls[rand.Intn(len(urls))]

	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}

	req.Header.Set("User-Agent", "") // Reduce header size.

	jar, _ := cookiejar.New(nil)
	proxy := &url.URL{Scheme: "socks5", Host: laddr}

	client := &http.Client{
		Transport: &http.Transport{
			Proxy: func(*http.Request) (*url.URL, error) { return proxy, nil },
		},
		Jar: jar,
	}
	defer client.CloseIdleConnections()

	done := ctx.Done()
	number := opts.Number
	sleep := time.Duration(0)

	for {
		ctx, cancel := context.WithTimeout(ctx, opts.Timeout)

		resp, err := client.Do(req.WithContext(ctx))
		if err != nil {
			number--

			sleep = 0
		} else {
			const maxBodySlurpSize = 2 << 10
			if resp.ContentLength == -1 || resp.ContentLength <= maxBodySlurpSize {
				_, _ = io.CopyN(ioutil.Discard, resp.Body, maxBodySlurpSize)
			}
			resp.Body.Close()
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
