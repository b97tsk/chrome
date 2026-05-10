package httpfs

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/b97tsk/chrome"
)

type Options struct {
	ListenAddr string `yaml:"on"`

	Dir chrome.EnvString

	handler http.Handler
}

type Service struct{}

const ServiceName = "httpfs"

func (Service) Name() string {
	return ServiceName
}

func (Service) Options() any {
	return new(Options)
}

func (Service) Run(ctx chrome.Context) {
	logger := ctx.Manager.Logger().With(slog.String("job", ctx.JobName))

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
			if h := (<-optsOut).handler; h != nil {
				h.ServeHTTP(rw, req)
				return
			}

			http.NotFound(rw, req)
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

		ln, err := net.Listen("tcp", (<-optsOut).ListenAddr)
		if err != nil {
			logger.Error("net:listen", slog.Any("error", err))
			return err
		}

		defer logger.Info("net:listening", slog.Any("addr", ln.Addr()))

		server = &http.Server{Handler: handler}
		serverDown = make(chan struct{})
		serverListener = ln

		go func(srv *http.Server, down chan<- struct{}) {
			_ = srv.Serve(ln)

			close(down)
		}(server, serverDown)

		return nil
	}

	stopServer := func() {
		if server == nil {
			return
		}

		defer logger.Info("net:listen:close", slog.Any("addr", serverListener.Addr()))

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
		case ev := <-ctx.Event:
			switch ev := ev.(type) {
			case chrome.LoadEvent:
				old := <-optsOut
				new := *ev.Options.(*Options)
				new.handler = old.handler

				if _, _, err := net.SplitHostPort(new.ListenAddr); err != nil {
					logger.Error("loading", slog.Any("error", err))
					return
				}

				if new.ListenAddr != old.ListenAddr {
					stopServer()
				}

				if new.Dir != old.Dir {
					new.handler = http.FileServer(http.Dir(new.Dir))
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
