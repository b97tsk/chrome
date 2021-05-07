package pprof

import (
	"net"
	"net/http"

	// Import for side-effects.
	_ "net/http/pprof"

	"github.com/b97tsk/chrome"
)

type Service struct{}

func (Service) Name() string {
	return "pprof"
}

func (Service) Options() interface{} {
	return nil
}

func (Service) Run(ctx chrome.Context) {
	ln, err := net.Listen("tcp", ctx.ListenAddr)
	if err != nil {
		ctx.Logger.Error(err)
		return
	}

	ctx.Logger.Infof("listening on %v", ln.Addr())
	defer ctx.Logger.Infof("stopped listening on %v", ln.Addr())

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
