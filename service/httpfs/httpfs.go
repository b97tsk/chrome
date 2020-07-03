package httpfs

import (
	"context"
	"log"
	"net"
	"net/http"

	"github.com/b97tsk/chrome/service"
	"gopkg.in/yaml.v2"
)

type Options struct {
	Dir service.String

	handler http.Handler
}

type Service struct{}

func (Service) Name() string {
	return "httpfs"
}

func (Service) Run(ctx service.Context) {
	ln, err := net.Listen("tcp", ctx.ListenAddr)
	if err != nil {
		log.Printf("[httpfs] %v\n", err)
		return
	}
	log.Printf("[httpfs] listening on %v\n", ln.Addr())
	defer log.Printf("[httpfs] stopped listening on %v\n", ln.Addr())

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
		server     *http.Server
		serverDown chan error
	)
	initialize := func() {
		if server != nil {
			return
		}
		server = &http.Server{
			Handler: http.HandlerFunc(
				func(rw http.ResponseWriter, req *http.Request) {
					opts := <-optsOut
					if opts.handler == nil {
						http.NotFound(rw, req)
						return
					}
					opts.handler.ServeHTTP(rw, req)
				},
			),
		}
		serverDown = make(chan error, 1)
		go func() {
			serverDown <- server.Serve(ln)
			close(serverDown)
		}()
	}
	defer func() {
		if server != nil {
			server.Shutdown(context.TODO())
			<-serverDown
		}
	}()

	for {
		select {
		case opts := <-ctx.Opts:
			if new, ok := opts.(Options); ok {
				old := <-optsOut
				new.handler = old.handler
				if new.Dir != old.Dir {
					new.handler = http.FileServer(http.Dir(new.Dir.String()))
				}
				optsIn <- new
				initialize()
			}
		case <-serverDown:
			return
		case <-ctx.Done:
			return
		}
	}
}

func (Service) UnmarshalOptions(text []byte) (interface{}, error) {
	var opts Options
	if err := yaml.UnmarshalStrict(text, &opts); err != nil {
		return nil, err
	}
	return opts, nil
}
