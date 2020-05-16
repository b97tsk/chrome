package shadowsocks

import (
	"context"
	"log"
	"net"
	"time"

	"github.com/b97tsk/chrome/internal/proxy"
	"github.com/b97tsk/chrome/internal/utility"
	"github.com/b97tsk/chrome/service"
	"github.com/shadowsocks/go-shadowsocks2/core"
	"github.com/shadowsocks/go-shadowsocks2/socks"
	"gopkg.in/yaml.v2"
)

type Options struct {
	Method   string
	Password string
	Proxy    service.ProxyChain `yaml:"over"`
}

type Service struct{}

func (Service) Name() string {
	return "shadowsocks"
}

func (Service) Run(ctx service.Context) {
	ln, err := net.Listen("tcp", ctx.ListenAddr)
	if err != nil {
		log.Printf("[shadowsocks] %v\n", err)
		return
	}
	log.Printf("[shadowsocks] listening on %v\n", ln.Addr())
	defer log.Printf("[shadowsocks] stopped listening on %v\n", ln.Addr())
	defer ln.Close()

	type X struct {
		Options
		Dialer proxy.Dialer
		Cipher core.Cipher
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
	}(X{Dialer: proxy.Direct})

	man := ctx.Manager

	man.ServeListener(ln, func(c net.Conn) {
		x, ok := <-xout
		if !ok {
			return
		}
		if x.Cipher == nil {
			time.Sleep(time.Second)
			x = <-xout
			if x.Cipher == nil {
				return
			}
		}

		c = x.Cipher.StreamConn(c)
		addr, err := socks.ReadAddr(c)
		if err != nil {
			log.Printf("[shadowsocks] read addr: %v\n", err)
			return
		}

		ctx, c := service.CheckConnectivity(context.Background(), c)

		rc, err := man.Dial(ctx, x.Dialer, "tcp", addr.String())
		if err != nil {
			// log.Printf("[shadowsocks] %v\n", err)
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
				if new.Method != old.Method || new.Password != old.Password {
					cipher, err := core.PickCipher(new.Method, nil, new.Password)
					if err != nil {
						log.Printf("[shadowsocks] fatal: pick cipher: %v\n", err)
						return
					}
					x.Cipher = cipher
				}
				if !new.Proxy.Equals(old.Proxy) {
					d, _ := new.Proxy.NewDialer(proxy.Direct)
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
