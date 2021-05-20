package pprof

import (
	"context"
	"net"
	"net/http"

	// Import for side-effects.
	_ "net/http/pprof"

	"github.com/b97tsk/chrome"
)

type Options struct {
	ListenAddr string `yaml:"on"`
}

type Service struct{}

const _ServiceName = "pprof"

func (Service) Name() string {
	return _ServiceName
}

func (Service) Options() interface{} {
	return new(Options)
}

func (Service) Run(ctx chrome.Context) {
	logger := ctx.Manager.Logger(_ServiceName)

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

	var (
		server         *http.Server
		serverDown     chan struct{}
		serverListener net.Listener
	)

	startServer := func() error {
		if server != nil {
			return nil
		}

		opts := <-optsOut

		ln, err := net.Listen("tcp", opts.ListenAddr)
		if err != nil {
			logger.Error(err)
			return err
		}

		defer logger.Infof("listening on %v", ln.Addr())

		server = &http.Server{}
		serverDown = make(chan struct{})
		serverListener = ln

		go func() {
			_ = server.Serve(ln)

			close(serverDown)
		}()

		return nil
	}

	stopServer := func() {
		if server == nil {
			return
		}

		defer logger.Infof("stopped listening on %v", serverListener.Addr())

		_ = server.Shutdown(context.Background())

		server = nil
		serverDown = nil
		serverListener = nil
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
