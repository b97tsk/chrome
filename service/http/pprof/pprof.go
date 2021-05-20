package pprof

import (
	"net"
	"net/http"

	// Import for side-effects.
	_ "net/http/pprof"

	"github.com/b97tsk/chrome"
)

type Service struct{}

const _ServiceName = "pprof"

func (Service) Name() string {
	return _ServiceName
}

func (Service) Options() interface{} {
	return nil
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

	go func() { _ = http.Serve(ln, nil) }()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ctx.Opts:
		}
	}
}
