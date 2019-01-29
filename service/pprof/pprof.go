package pprof

import (
	"log"
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

func (Service) Aliases() []string {
	return nil
}

func (Service) Run(ctx service.Context) {
	ln, err := net.Listen("tcp", ctx.ListenAddr)
	if err != nil {
		log.Printf("[pprof] %v\n", err)
		return
	}
	log.Printf("[pprof] listening on %v\n", ln.Addr())
	defer log.Printf("[pprof] stopped listening on %v\n", ln.Addr())
	defer ln.Close()

	go http.Serve(ln, nil)

	for {
		select {
		case <-ctx.Events:
		case <-ctx.Done:
			return
		}
	}
}

func (Service) UnmarshalOptions(text []byte) (interface{}, error) {
	return Options{}, nil
}
