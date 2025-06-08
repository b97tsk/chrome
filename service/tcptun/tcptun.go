package tcptun

import (
	"context"
	"net"

	"github.com/b97tsk/chrome"
	"github.com/b97tsk/proxy"
)

type Options struct {
	ListenAddr  string `yaml:"on"`
	ForwardAddr string `yaml:"for"`

	Proxy chrome.Proxy `yaml:"over"`

	Conn  chrome.ConnOptions
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
	logger := ctx.Manager.Logger(ctx.JobName)

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

		go ctx.Manager.Serve(ln, func(local net.Conn) {
			opts, ok := <-optsOut
			if !ok {
				return
			}

			getRemote := func(ctx context.Context) (net.Conn, error) {
				opts := <-optsOut
				if opts.ForwardAddr == "" {
					return nil, chrome.CloseConn
				}

				return proxy.Dial(ctx, opts.Proxy.Dialer(), "tcp", opts.ForwardAddr)
			}

			remote := ctx.Manager.NewConn(opts.ForwardAddr, getRemote, opts.Conn, opts.Relay, logger, nil)
			defer remote.Close()

			ctx.Manager.Relay(local, remote, opts.Relay)
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
		case ev := <-ctx.Event:
			switch ev := ev.(type) {
			case chrome.LoadEvent:
				old := <-optsOut
				new := *ev.Options.(*Options)

				if _, _, err := net.SplitHostPort(new.ListenAddr); err != nil {
					logger.Error(err)
					return
				}

				if new.ListenAddr != old.ListenAddr {
					stopServer()
				}

				optsIn <- new
			case chrome.LoadedEvent:
				if err := startServer(); err != nil {
					return
				}
			}
		}
	}
}
