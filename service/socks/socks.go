package socks

import (
	"context"
	"log"
	"net"
	"sync/atomic"

	"github.com/b97tsk/chrome/internal/proxy"
	"github.com/b97tsk/chrome/internal/utility"
	"github.com/b97tsk/chrome/service"
	"github.com/shadowsocks/go-shadowsocks2/socks"
	"gopkg.in/yaml.v2"
)

type Options struct {
	Proxy service.ProxyChain `yaml:"over"`
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

	man := ctx.Manager

	var dial atomic.Value
	dial.Store(
		func(ctx context.Context, network, addr string) (net.Conn, error) {
			return man.Dial(ctx, proxy.Direct, network, addr)
		},
	)

	man.ServeListener(ln, func(c net.Conn) {
		addr, err := socks.Handshake(c)
		if err != nil {
			log.Printf("[socks] socks handshake: %v\n", err)
			return
		}

		dial := dial.Load().(func(context.Context, string, string) (net.Conn, error))
		ctx, c := service.CheckConnectivity(context.Background(), c)
		rc, err := dial(ctx, "tcp", addr.String())
		if err != nil {
			// log.Printf("[socks] %v\n", err)
			return
		}
		defer rc.Close()

		utility.Relay(rc, c)
	})

	var options Options

	for {
		select {
		case data := <-ctx.Events:
			if new, ok := data.(Options); ok {
				old := options
				options = new
				if !new.Proxy.Equals(old.Proxy) {
					d, _ := new.Proxy.NewDialer(proxy.Direct)
					dial.Store(
						func(ctx context.Context, network, addr string) (net.Conn, error) {
							return man.Dial(ctx, d, network, addr)
						},
					)
				}
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
