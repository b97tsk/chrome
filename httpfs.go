package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"sync/atomic"

	"gopkg.in/yaml.v2"
)

type httpfsOptions struct {
	Dir string
}

type httpfsService struct{}

func (httpfsService) Run(ctx ServiceCtx) {
	ln, err := net.Listen("tcp", ctx.Name)
	if err != nil {
		log.Printf("[httpfs] %v\n", err)
		return
	}
	log.Printf("[httpfs] listening on %v\n", ln.Addr())
	defer log.Printf("[httpfs] stopped listening on %v\n", ln.Addr())

	var handler atomic.Value

	server := http.Server{
		Handler: http.HandlerFunc(
			func(rw http.ResponseWriter, req *http.Request) {
				handler := handler.Load()
				if handler == nil {
					http.NotFound(rw, req)
					return
				}
				handler.(http.Handler).ServeHTTP(rw, req)
			},
		),
	}

	serverDown := make(chan error, 1)
	defer func() {
		server.Shutdown(context.TODO())
		<-serverDown
	}()

	go func() {
		serverDown <- server.Serve(ln)
		close(serverDown)
	}()

	var options httpfsOptions

	for {
		select {
		case data := <-ctx.Events:
			if new, ok := data.(httpfsOptions); ok {
				old := options
				options = new
				if new.Dir != old.Dir {
					handler.Store(http.FileServer(http.Dir(os.ExpandEnv(new.Dir))))
				}
			}
		case err := <-serverDown:
			log.Printf("[httpfs] %v\n", err)
			return
		case <-ctx.Done:
			return
		}
	}
}

func (httpfsService) UnmarshalOptions(text []byte) (interface{}, error) {
	var options httpfsOptions
	if err := yaml.UnmarshalStrict(text, &options); err != nil {
		return nil, err
	}
	return options, nil
}

func init() {
	services.Add("httpfs", httpfsService{})
}