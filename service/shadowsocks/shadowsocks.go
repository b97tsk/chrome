package shadowsocks

import (
	"context"
	"log"
	"net"
	"strings"
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
	Proxy    proxy.ProxyChain `yaml:"over"`
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
	}(X{Dialer: service.Direct})

	ctx.Manager.ServeListener(ln, func(c net.Conn) {
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

		rc, err := proxy.Dial(ctx, x.Dialer, "tcp", addr.String())
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
					password := new.Password
					if strings.HasPrefix(password, "base64:") {
						bytes, err := utility.DecodeBase64String(password[7:])
						if err != nil {
							log.Println("[shadowsocks] fatal: password is not a valid base64 string")
							return
						}
						password = string(bytes)
					}
					cipher, err := core.PickCipher(new.Method, nil, password)
					if err != nil {
						log.Printf("[shadowsocks] fatal: pick cipher: %v\n", err)
						return
					}
					x.Cipher = cipher
				}
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
