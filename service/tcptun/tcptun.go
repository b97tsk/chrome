package tcptun

import (
	"context"
	"log"
	"net"
	"time"

	"github.com/b97tsk/chrome/internal/proxy"
	"github.com/b97tsk/chrome/service"
	"gopkg.in/yaml.v2"
)

type Options struct {
	ForwardAddr string             `yaml:"for"`
	Proxy       service.ProxyChain `yaml:"over"`
	Dial        struct {
		Timeout time.Duration
	}

	dialer proxy.Dialer
}

type Service struct{}

func (Service) Name() string {
	return "tcptun"
}

func (Service) Run(ctx service.Context) {
	ln, err := net.Listen("tcp", ctx.ListenAddr)
	if err != nil {
		log.Printf("[tcptun] %v\n", err)
		return
	}
	log.Printf("[tcptun] listening on %v\n", ln.Addr())
	defer log.Printf("[tcptun] stopped listening on %v\n", ln.Addr())
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

	man := ctx.Manager

	man.ServeListener(ln, func(c net.Conn) {
		opts, ok := <-optsOut
		if !ok {
			return
		}
		if opts.ForwardAddr == "" {
			time.Sleep(time.Second)
			opts = <-optsOut
			if opts.ForwardAddr == "" {
				return
			}
		}

		ctx, c := service.CheckConnectivity(context.Background(), c)

		rc, err := man.Dial(ctx, opts.dialer, "tcp", opts.ForwardAddr, opts.Dial.Timeout)
		if err != nil {
			// log.Printf("[tcptun] %v\n", err)
			return
		}
		defer rc.Close()

		service.Relay(rc, c)
	})

	for {
		select {
		case data := <-ctx.Events:
			if new, ok := data.(Options); ok {
				old := <-optsOut
				new.dialer = old.dialer
				if !new.Proxy.Equals(old.Proxy) {
					new.dialer, _ = new.Proxy.NewDialer()
				}
				optsIn <- new
			}
		case <-ctx.Done:
			return
		}
	}
}

func (Service) UnmarshalOptions(text []byte) (interface{}, error) {
	var options Options
	if err := yaml.UnmarshalStrict(text, &options); err != nil {
		return nil, err
	}
	return options, nil
}
