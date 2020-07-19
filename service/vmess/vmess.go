package vmess

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"time"

	"github.com/b97tsk/chrome/internal/v2ray"
	"github.com/b97tsk/chrome/service"
	"github.com/shadowsocks/go-shadowsocks2/socks"
	"gopkg.in/yaml.v2"
)

type Options struct {
	Address string `yaml:"add"`
	Port    string `yaml:"port"`
	ID      string `yaml:"id"`
	AlterID string `yaml:"aid"`

	Net  string
	TLS  string
	Type string

	Path string
	Host string

	Mux struct {
		Enabled     bool
		Concurrency int
		Ping        PingOptions
	}

	Dial struct {
		Timeout time.Duration
	}

	instance *v2ray.Instance
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
	return "vmess"
}

func (Service) Run(ctx service.Context) {
	ln, err := net.Listen("tcp", ctx.ListenAddr)
	if err != nil {
		log.Printf("[vmess] %v\n", err)
		return
	}
	log.Printf("[vmess] listening on %v\n", ln.Addr())
	defer log.Printf("[vmess] stopped listening on %v\n", ln.Addr())
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
				log.Printf("[vmess] socks handshake: %v\n", err)
				return
			}

			ctx, c := service.CheckConnectivity(context.Background(), c)
			rc, err := man.Dial(ctx, opts.instance, "tcp", addr.String(), opts.Dial.Timeout)
			if err != nil {
				// log.Printf("[vmess] %v\n", err)
				return
			}
			defer rc.Close()

			service.Relay(rc, c)
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
				log.Printf("[vmess] close instance: %v\n", err)
			}
			instance = nil
		}
	}
	defer stopInstance()

	startInstance := func(opts Options) {
		i, err := createInstance(opts)
		if err != nil {
			log.Printf("[vmess] create instance: %v\n", err)
			return
		}
		if err := i.Start(); err != nil {
			log.Printf("[vmess] start instance: %v\n", err)
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
				if new != old {
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

func createInstance(opts Options) (*v2ray.Instance, error) {
	if opts.Mux.Enabled {
		if opts.Mux.Concurrency < 1 {
			opts.Mux.Concurrency = defaultMuxConcurrency
		}
		if opts.Mux.Ping.Enabled {
			opts.Mux.Concurrency++
		}
	}

	var templateName string

	switch opts.Net + "/" + opts.TLS {
	case "kcp/", "kcp/none":
		if opts.Type == "" {
			opts.Type = "none"
		}
		templateName = "kcp"
	case "tcp/", "tcp/none":
		templateName = "tcp"
		if opts.Type == "http" {
			if opts.Path == "" {
				opts.Path = "/"
			}
			if opts.Host == "" && net.ParseIP(opts.Address) == nil {
				opts.Host = opts.Address
			}
			templateName = "tcp/http"
		}
	case "tcp/tls":
		if opts.Host == "" && net.ParseIP(opts.Address) == nil {
			opts.Host = opts.Address
		}
		templateName = "tcp/tls"
	case "ws/", "ws/none":
		templateName = "ws"
	case "h2/tls", "ws/tls":
		if opts.Path == "" {
			opts.Path = "/"
		}
		if opts.Host == "" && net.ParseIP(opts.Address) == nil {
			opts.Host = opts.Address
		}
		templateName = opts.Net + "/" + opts.TLS
	}

	tpl := vmessTemplate.Lookup(templateName)
	if tpl == nil {
		return nil, errors.New("unknown vmess type: " + templateName)
	}

	buf := new(bytes.Buffer)

	if err := tpl.Execute(buf, &opts); err != nil {
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
		log.Printf("[vmess] ping: %v\n", err)
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
	defaultMuxConcurrency    = 8
	defaultPingURL           = "https://www.google.com/"
	defaultPingNumber        = 4
	defaultPingTimeout       = 5 * time.Second
	defaultPingIntervalStart = 1 * time.Second
	defaultPingIntervalStep  = 2 * time.Second
	defaultPingIntervalMax   = 11 * time.Second
)
