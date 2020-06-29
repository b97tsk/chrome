package socks

import (
	"context"
	"log"
	"net"
	"time"

	"github.com/b97tsk/chrome/internal/proxy"
	"github.com/b97tsk/chrome/service"
	"github.com/shadowsocks/go-shadowsocks2/socks"
	"gopkg.in/yaml.v2"
)

type Options struct {
	Proxy service.ProxyChain `yaml:"over"`
	Dial  struct {
		Timeout time.Duration
	}

	dialer proxy.Dialer
}

type Service struct{}

func (Service) Name() string {
	return "socks"
}

func (Service) Run(ctx service.Context) {
	ln, err := net.Listen("tcp", ctx.ListenAddr)
	if err != nil {
		log.Printf("[socks] %v\n", err)
		return
	}
	log.Printf("[socks] listening on %v\n", ln.Addr())
	defer log.Printf("[socks] stopped listening on %v\n", ln.Addr())
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

		addr, err := socks.Handshake(c)
		if err != nil {
			log.Printf("[socks] socks handshake: %v\n", err)
			return
		}

		ctx, c := service.CheckConnectivity(context.Background(), c)
		rc, err := man.Dial(ctx, opts.dialer, "tcp", addr.String(), opts.Dial.Timeout)
		if err != nil {
			// log.Printf("[socks] %v\n", err)
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
