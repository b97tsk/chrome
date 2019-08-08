package tcptun

import (
	"context"
	"log"
	"net"
	"time"

	"github.com/b97tsk/chrome/internal/proxy"
	"github.com/b97tsk/chrome/internal/utility"
	"github.com/b97tsk/chrome/service"
	"gopkg.in/yaml.v2"
)

type Options struct {
	ForwardAddr string           `yaml:"for"`
	Proxy       proxy.ProxyChain `yaml:"over"`
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

	type X struct {
		Options
		Dialer proxy.Dialer
	}
	xin, xout := make(chan X), make(chan X)
	defer func() {
		close(xin)
		for range xout {
			// Let's wait until xout closes.
			// Though this is not necessary.
		}
	}()
	go func(x X) {
		ok := true
		for ok {
			select {
			case x, ok = <-xin:
			case xout <- x:
			}
		}
		close(xout)
	}(X{Dialer: service.Direct})

	ctx.Manager.ServeListener(ln, func(c net.Conn) {
		x, ok := <-xout
		if !ok {
			return
		}
		if x.ForwardAddr == "" {
			time.Sleep(time.Second)
			x = <-xout
			if x.ForwardAddr == "" {
				return
			}
		}

		ctx, c := service.CheckConnectivity(context.Background(), c)

		rc, err := proxy.Dial(ctx, x.Dialer, "tcp", x.ForwardAddr)
		if err != nil {
			// log.Printf("[tcptun] %v\n", err)
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
				x := <-xout
				x.Options = new
				if !new.Proxy.Equals(old.Proxy) {
					d, _ := new.Proxy.NewDialer(service.Direct)
					x.Dialer = d
				}
				xin <- x
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
