package pprof

import (
	"net"
	"net/http"
	_ "net/http/pprof"

	"github.com/b97tsk/chrome/service"
)

type Options struct{}

type Service struct{}

func (Service) Name() string {
	return "pprof"
}

func (Service) Run(ctx service.Context) {
	ln, err := net.Listen("tcp", ctx.ListenAddr)
	if err != nil {
		ctx.Logger.Print(err)
		return
	}
	ctx.Logger.Printf("listening on %v", ln.Addr())
	defer ctx.Logger.Printf("stopped listening on %v", ln.Addr())
	defer ln.Close()

	go http.Serve(ln, nil)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ctx.Opts:
		}
	}
}

func (Service) UnmarshalOptions(text []byte) (interface{}, error) {
	return Options{}, nil
}
