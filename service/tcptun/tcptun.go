package tcptun

import (
	"context"
	"net"

	"github.com/b97tsk/chrome"
)

type Options struct {
	ListenAddr  string `yaml:"on"`
	ForwardAddr string `yaml:"for"`

	Proxy chrome.Proxy `yaml:"over"`

	Dial  chrome.DialOptions
	Relay chrome.RelayOptions
}

type Service struct{}

const ServiceName = "tcptun"

func (Service) Name() string {
	return ServiceName
}

func (Service) Options() any {
	return new(Options)
}

func (Service) Run(ctx chrome.Context) {
	logger := ctx.Manager.Logger(ServiceName)

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

	var server net.Listener

	startServer := func() error {
		if server != nil {
			return nil
		}

		ln, err := net.Listen("tcp", (<-optsOut).ListenAddr)
		if err != nil {
			logger.Error(err)
			return err
		}

		defer logger.Infof("listening on %v", ln.Addr())

		server = ln

		go ctx.Manager.Serve(ln, func(c net.Conn) {
			getRemote := func(localCtx context.Context) net.Conn {
				opts := <-optsOut
				if opts.ForwardAddr == "" {
					return nil
				}

				getopts := func() (chrome.Proxy, chrome.DialOptions, bool) {
					opts, ok := <-optsOut
					return opts.Proxy, opts.Dial, ok
				}

				remote, _ := ctx.Manager.Dial(localCtx, "tcp", opts.ForwardAddr, getopts, logger)

				return remote
			}

			ctx.Manager.Relay(c, getRemote, nil, (<-optsOut).Relay)
		})

		return nil
	}

	stopServer := func() {
		if server == nil {
			return
		}

		defer logger.Infof("stopped listening on %v", server.Addr())

		_ = server.Close()
		server = nil
	}
	defer stopServer()

	for {
		select {
		case <-ctx.Done():
			return
		case opts := <-ctx.Load:
			if new, ok := opts.(*Options); ok {
				old := <-optsOut
				new := *new

				if _, _, err := net.SplitHostPort(new.ListenAddr); err != nil {
					logger.Error(err)
					return
				}

				if new.ListenAddr != old.ListenAddr {
					stopServer()
				}

				optsIn <- new
			}
		case <-ctx.Loaded:
			if err := startServer(); err != nil {
				return
			}
		}
	}
}
