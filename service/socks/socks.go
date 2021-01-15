package socks

import (
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
		ctx.Logger.Error(err)
		return
	}

	ctx.Logger.Infof("listening on %v", ln.Addr())
	defer ctx.Logger.Infof("stopped listening on %v", ln.Addr())

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

		ctx.Manager.ServeListener(ln, func(c net.Conn) {
			opts, ok := <-optsOut
			if !ok {
				return
			}

			addr, err := socks.Handshake(c)
			if err != nil {
				ctx.Logger.Debugf("socks handshake: %v", err)
				return
			}

			local, localCtx := service.NewConnChecker(c)

			remote, err := ctx.Manager.Dial(localCtx, opts.dialer, "tcp", addr.String(), opts.Dial.Timeout)
			if err != nil {
				ctx.Logger.Trace(err)
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
				new.dialer = old.dialer

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
