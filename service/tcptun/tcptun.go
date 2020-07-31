package tcptun

import (
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
		writeLog(err)
		return
	}
	writeLogf("listening on %v", ln.Addr())
	defer writeLogf("stopped listening on %v", ln.Addr())
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
			if !ok || opts.ForwardAddr == "" {
				return
			}

			local, ctx := service.NewConnChecker(c)

			remote, err := man.Dial(ctx, opts.dialer, "tcp", opts.ForwardAddr, opts.Dial.Timeout)
			if err != nil {
				// writeLog(err)
				return
			}
			defer remote.Close()

			service.Relay(local, remote)
		})
	}

	for {
		select {
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
		case <-ctx.Done:
			return
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
