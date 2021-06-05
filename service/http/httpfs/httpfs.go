package httpfs

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/b97tsk/chrome"
	"github.com/b97tsk/log"
)

type Options struct {
	ListenAddr string `yaml:"on"`

	Dir chrome.EnvString

	handler http.Handler
}

type Service struct{}

const _ServiceName = "httpfs"

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

	handler := http.HandlerFunc(
		func(rw http.ResponseWriter, req *http.Request) {
			opts := <-optsOut
			if opts.handler == nil {
				http.NotFound(rw, req)
				return
			}
			opts.handler.ServeHTTP(rw, req)
		},
	)

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

		server = &http.Server{
			Handler:  handler,
			ErrorLog: logger.Get(log.LevelDebug),
		}
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

		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()

		_ = server.Shutdown(ctx)

		server = nil
		serverDown = nil
		serverListener = nil
	}

	defer stopServer()

	for {
		select {
		case <-ctx.Done():
			return
		case <-serverDown:
			return
		case opts := <-ctx.Load:
			if new, ok := opts.(*Options); ok {
				old := <-optsOut
				new := *new
				new.handler = old.handler

				if new.ListenAddr != old.ListenAddr {
					stopServer()
				}

				if new.Dir != old.Dir {
					new.handler = http.FileServer(http.Dir(new.Dir.String()))
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
