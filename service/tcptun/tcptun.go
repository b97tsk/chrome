package tcptun

import (
	"net"
	"time"

	"github.com/b97tsk/chrome"
	"github.com/b97tsk/chrome/internal/proxy"
)

type Options struct {
	ForwardAddr string            `yaml:"for"`
	Proxy       chrome.ProxyChain `yaml:"over"`
	Dial        struct {
		Timeout time.Duration
	}

	dialer proxy.Dialer
}

type Service struct{}

const _ServiceName = "tcptun"

func (Service) Name() string {
	return _ServiceName
}

func (Service) Options() interface{} {
	return new(Options)
}

func (Service) Run(ctx chrome.Context) {
	logger := ctx.Manager.Logger(_ServiceName)

	ln, err := net.Listen("tcp", ctx.ListenAddr)
	if err != nil {
		logger.Error(err)
		return
	}

	logger.Infof("listening on %v", ln.Addr())
	defer logger.Infof("stopped listening on %v", ln.Addr())

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
			if !ok || opts.ForwardAddr == "" {
				return
			}

			local, localCtx := chrome.NewConnChecker(c)

			remote, err := ctx.Manager.Dial(localCtx, opts.dialer, "tcp", opts.ForwardAddr, opts.Dial.Timeout)
			if err != nil {
				logger.Trace(err)
				return
			}
			defer remote.Close()

			chrome.Relay(local, remote)
		})
	}

	for {
		select {
		case <-ctx.Done():
			return
		case opts := <-ctx.Opts:
			if new, ok := opts.(*Options); ok {
				old := <-optsOut
				new := *new
				new.dialer = old.dialer

				if !new.Proxy.Equals(old.Proxy) {
					new.dialer = new.Proxy.NewDialer()
				}

				optsIn <- new

				initialize()
			}
		}
	}
}
