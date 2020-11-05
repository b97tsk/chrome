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
	"sync/atomic"
	"time"

	"github.com/b97tsk/chrome/internal/v2ray"
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

	Debug bool

	stats statsINFO
}

type statsINFO struct {
	ins        *v2ray.Instance
	ctx        context.Context
	cancel     context.CancelFunc
	readevents chan struct{}
}

type ProtocolOptions struct {
	FREEDOM struct {
		DomainStrategy string `json:"domainStrategy,omitempty"`
		Redirect       string `json:"redirect,omitempty"`
	}
	VMESS struct {
		Address string
		Port    int `yaml:"-"`
		ID      string
		AlterID int `yaml:"aid"`
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
		ServerName string `json:"serverName"`
	}
	WS struct {
		Path   string
		Header map[string]string
	}
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
		writeLog(err)
		return
	}
	writeLogf("listening on %v", ln.Addr())
	defer writeLogf("stopped listening on %v", ln.Addr())
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

		var seqno int32
		man := ctx.Manager
		man.ServeListener(ln, func(c net.Conn) {
			opts, ok := <-optsOut
			if !ok || opts.stats.ins == nil {
				return
			}

			addr, err := socks.Handshake(c)
			if err != nil {
				writeLogf("socks handshake: %v", err)
				return
			}

			local, ctx := service.NewConnCheckerContext(opts.stats.ctx, c)

			remote, err := man.Dial(ctx, opts.stats.ins, "tcp", addr.String(), opts.Dial.Timeout)
			if err != nil {
				// writeLog(err)
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

			if opts.Debug {
				prefix := fmt.Sprintf("[v2ray] (%v) ", atomic.AddInt32(&seqno, 1))
				remote = service.NewConnLogger(remote, prefix)
			}

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
						writeLogf("close instance: %v", err)
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
			writeLogf("create instance: %v", err)
			return
		}
		if err := i.Start(); err != nil {
			writeLogf("start instance: %v", err)
			return
		}
		stats.ins = i
		stats.ctx, stats.cancel = context.WithCancel(context.Background())
		stats.readevents = make(chan struct{})
		if opts.Mux.Enabled && opts.Mux.Ping.Enabled {
			laddr := ctx.ListenAddr
			ctx, cancel := context.WithCancel(context.Background())
			cancelPing = cancel
			if restart == nil {
				restart = make(chan struct{})
			}
			go startPing(ctx, opts.Mux.Ping, laddr, restart)
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

func createInstance(opts Options) (*v2ray.Instance, error) {
	if err := parseURL(&opts); err != nil {
		return nil, err
	}

	opts.Protocol = "freedom"
	opts.Transport = "tcp"

	for _, typ := range strings.SplitN(opts.Type, "+", 2) {
		switch typ {
		case "freedom", "vmess":
			opts.Protocol = typ
			switch typ {
			case "vmess":
				host, port, err := net.SplitHostPort(opts.VMESS.Address)
				if err != nil {
					return nil, errors.New("invalid address: " + opts.VMESS.Address)
				}
				opts.VMESS.Port, err = strconv.Atoi(port)
				if err != nil {
					return nil, errors.New("invalid port in address: " + opts.VMESS.Address)
				}
				opts.VMESS.Address = host
			}
		case "http", "kcp", "tcp", "tcp/tls", "ws", "ws/tls":
			opts.Transport = typ
			switch typ {
			case "kcp":
				if opts.KCP.Header == "" {
					opts.KCP.Header = "none"
				}
			}
		default:
			return nil, errors.New("unknown type: " + opts.Type)
		}
	}

	if opts.Mux.EnabledByYAML != nil {
		opts.Mux.Enabled = *opts.Mux.EnabledByYAML
	} else {
		if opts.Protocol == "vmess" {
			opts.Mux.Enabled = true
		}
	}

	var buf bytes.Buffer

	if err := v2rayTemplate.Execute(&buf, &opts); err != nil {
		return nil, err
	}

	return v2ray.NewInstanceFromJSON(buf.Bytes())
}

func parseURL(opts *Options) error {
	if opts.URL == "" {
		return nil
	}

	b64 := strings.TrimPrefix(opts.URL, "vmess://")
	if b64 == opts.URL {
		return errors.New("url should start with vmess://")
	}

	b64 = strings.ReplaceAll(b64, "-", "+")
	b64 = strings.ReplaceAll(b64, "_", "/")

	enc := base64.StdEncoding
	if len(b64)%4 != 0 {
		enc = base64.RawStdEncoding
	}
	data, err := enc.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("decode vmess url: %w", err)
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
		return fmt.Errorf("unmarshal decoded vmess url: %w", err)
	}

	if unquote(string(config.Version)) == "" {
		fields := strings.SplitN(config.Path, ";", 2)
		if len(fields) == 2 {
			config.Path, config.Host = fields[0], fields[1]
		}
	}

	var transport string
	switch config.Net {
	case "http", "h2":
		transport = "http"
		if config.Host != "" {
			opts.HTTP.Host = []string{config.Host}
			if config.Host != config.Address {
				opts.TLS.ServerName = config.Host
			}
		}
		opts.HTTP.Path = config.Path
	case "kcp":
		transport = "kcp"
		opts.KCP.Header = config.Type
	case "tcp":
		transport = "tcp"
		if config.Type != "" && config.Type != "none" {
			return fmt.Errorf(`unknown type field "%v" in vmess url "%v"`, config.Type, opts.URL)
		}
		if config.TLS == "tls" {
			transport = "tcp/tls"
			if config.Host != "" && config.Host != config.Address {
				opts.TLS.ServerName = config.Host
			}
		}
	case "ws":
		transport = "ws"
		if config.TLS == "tls" {
			transport = "ws/tls"
			if config.Host != "" && config.Host != config.Address {
				opts.TLS.ServerName = config.Host
			}
		}
		opts.WS.Path = config.Path
	default:
		return fmt.Errorf(`unknown net field "%v" in vmess url "%v"`, config.Net, opts.URL)
	}

	opts.Type = "vmess+" + transport
	opts.VMESS.Address = net.JoinHostPort(config.Address, unquote(string(config.Port)))
	opts.VMESS.ID = config.ID
	opts.VMESS.AlterID, _ = strconv.Atoi(unquote(string(config.AlterID)))
	return nil
}

func startPing(ctx context.Context, opts PingOptions, laddr string, restart chan<- struct{}) {
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
		writeLogf("ping: %v", err)
		return
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
				io.CopyN(ioutil.Discard, resp.Body, maxBodySlurpSize)
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
			return
		}
		sleep := sleep
		if sleep < 1 {
			sleep = opts.Interval.Start
		}
		select {
		case <-done:
			return
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
