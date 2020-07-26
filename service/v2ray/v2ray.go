package v2ray

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/b97tsk/chrome/internal/v2ray"
	"github.com/b97tsk/chrome/service"
	"github.com/shadowsocks/go-shadowsocks2/socks"
	"gopkg.in/yaml.v2"
)

type Options struct {
	Type      string
	Protocol  string `yaml:"-"`
	Transport string `yaml:"-"`

	ProtocolOptions  `yaml:",inline"`
	TransportOptions `yaml:",inline"`

	Mux struct {
		Enabled     bool        `json:"enabled"`
		Concurrency int         `json:"concurrency,omitempty"`
		Ping        PingOptions `json:"-"`
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

	instance *v2ray.Instance
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
		log.Printf("[v2ray] %v\n", err)
		return
	}
	log.Printf("[v2ray] listening on %v\n", ln.Addr())
	defer log.Printf("[v2ray] stopped listening on %v\n", ln.Addr())
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

		man := ctx.Manager
		man.ServeListener(ln, func(c net.Conn) {
			opts, ok := <-optsOut
			if !ok || opts.instance == nil {
				return
			}

			addr, err := socks.Handshake(c)
			if err != nil {
				log.Printf("[v2ray] socks handshake: %v\n", err)
				return
			}

			local, ctx := service.NewConnChecker(c)

			remote, err := man.Dial(ctx, opts.instance, "tcp", addr.String(), opts.Dial.Timeout)
			if err != nil {
				// log.Printf("[v2ray] %v\n", err)
				return
			}
			defer remote.Close()

			service.Relay(local, remote)
		})
	}

	var (
		instance   *v2ray.Instance
		restart    chan struct{}
		cancelPing context.CancelFunc
	)

	stopInstance := func() {
		if cancelPing != nil {
			cancelPing()
			cancelPing = nil
		}
		if instance != nil {
			err := instance.Close()
			if err != nil {
				log.Printf("[v2ray] close instance: %v\n", err)
			}
			instance = nil
		}
	}
	defer stopInstance()

	startInstance := func(opts Options) {
		i, err := createInstance(opts)
		if err != nil {
			log.Printf("[v2ray] create instance: %v\n", err)
			return
		}
		if err := i.Start(); err != nil {
			log.Printf("[v2ray] start instance: %v\n", err)
			return
		}
		instance = i
		if opts.Mux.Enabled && opts.Mux.Ping.Enabled {
			ctx, cancel := context.WithCancel(context.Background())
			cancelPing = cancel
			if restart == nil {
				restart = make(chan struct{})
			}
			go startPing(ctx, opts.Mux.Ping, instance, restart)
		}
	}

	for {
		select {
		case opts := <-ctx.Opts:
			if new, ok := opts.(Options); ok {
				old := <-optsOut
				new.instance = old.instance
				if shouldRestart(old, new) {
					stopInstance()
					startInstance(new)
					new.instance = instance
				}
				optsIn <- new
				initialize()
			}
		case <-ctx.Done:
			return
		case <-restart:
			opts := <-optsOut
			stopInstance()
			startInstance(opts)
			opts.instance = instance
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

func shouldRestart(x, y Options) bool {
	var z Options
	x.Dial, y.Dial = z.Dial, z.Dial
	x.instance, y.instance = nil, nil
	return !reflect.DeepEqual(x, y)
}

func createInstance(opts Options) (*v2ray.Instance, error) {
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

	var buf bytes.Buffer

	if err := v2rayTemplate.Execute(&buf, &opts); err != nil {
		return nil, err
	}

	return v2ray.NewInstanceFromJSON(buf.Bytes())
}

func startPing(ctx context.Context, opts PingOptions, instance *v2ray.Instance, restart chan<- struct{}) {
	if opts.URL == "" {
		opts.URL = defaultPingURL
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
	req, err := http.NewRequest(http.MethodHead, opts.URL, nil)
	if err != nil {
		log.Printf("[v2ray] ping: %v\n", err)
		return
	}
	req.Header.Set("User-Agent", "") // Reduce header size.
	jar, err := cookiejar.New(nil)
	if err != nil {
		log.Printf("[v2ray] ping: %v\n", err)
		return
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: instance.DialContext,
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

const (
	defaultPingURL           = "https://www.google.com/"
	defaultPingNumber        = 4
	defaultPingTimeout       = 5 * time.Second
	defaultPingIntervalStart = 1 * time.Second
	defaultPingIntervalStep  = 2 * time.Second
	defaultPingIntervalMax   = 11 * time.Second
)
