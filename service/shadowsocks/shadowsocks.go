package shadowsocks

import (
	"net"
	"time"

	"github.com/b97tsk/chrome/internal/proxy"
	"github.com/b97tsk/chrome/service"
	"github.com/shadowsocks/go-shadowsocks2/core"
	"github.com/shadowsocks/go-shadowsocks2/socks"
	"gopkg.in/yaml.v2"
)

type Options struct {
	Method   string
	Password string
	Proxy    service.ProxyChain `yaml:"over"`
	Dial     struct {
		Timeout time.Duration
	}

	dialer proxy.Dialer
	cipher core.Cipher
}

type Service struct{}

func (Service) Name() string {
	return "shadowsocks"
}

func (Service) Run(ctx service.Context) {
	ln, err := net.Listen("tcp", ctx.ListenAddr)
	if err != nil {
		ctx.Logger.Print(err)
		return
	}
	ctx.Logger.Printf("listening on %v", ln.Addr())
	defer ctx.Logger.Printf("stopped listening on %v", ln.Addr())
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
			if !ok || opts.cipher == nil {
				return
			}

			c = opts.cipher.StreamConn(c)
			addr, err := socks.ReadAddr(c)
			if err != nil {
				ctx.Logger.Printf("read addr: %v", err)
				return
			}

			local, ctx := service.NewConnChecker(c)

			remote, err := man.Dial(ctx, opts.dialer, "tcp", addr.String(), opts.Dial.Timeout)
			if err != nil {
				// ctx.Logger.Print(err)
				return
			}
			defer remote.Close()

			service.Relay(local, remote)
		})
	}

	for {
		select {
		case <-ctx.Done():
			return
		case opts := <-ctx.Opts:
			if new, ok := opts.(Options); ok {
				old := <-optsOut
				new.cipher = old.cipher
				new.dialer = old.dialer
				if new.Method != old.Method || new.Password != old.Password {
					cipher, err := core.PickCipher(new.Method, nil, new.Password)
					if err != nil {
						ctx.Logger.Printf("fatal: pick cipher: %v", err)
						return
					}
					new.cipher = cipher
				}
				if !new.Proxy.Equals(old.Proxy) {
					new.dialer, _ = new.Proxy.NewDialer()
				}
				optsIn <- new
				initialize()
			}
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
