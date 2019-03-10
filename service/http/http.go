package http

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/b97tsk/chrome/configure"
	"github.com/b97tsk/chrome/internal/utility"
	"github.com/b97tsk/chrome/service"
	"gopkg.in/yaml.v2"
)

type Options struct {
	ProxyList service.ProxyList `yaml:"over"`
}

type Service struct{}

func (Service) Name() string {
	return "http"
}

func (Service) Aliases() []string {
	return nil
}

func (Service) Run(ctx service.Context) {
	ln, err := net.Listen("tcp", ctx.ListenAddr)
	if err != nil {
		log.Printf("[http] %v\n", err)
		return
	}
	log.Printf("[http] listening on %v\n", ln.Addr())
	defer log.Printf("[http] stopped listening on %v\n", ln.Addr())

	handler := NewHandler()
	server := http.Server{
		Handler:      handler,
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)), // Disable HTTP/2.
	}
	defer handler.CloseIdleConnections()

	serverDown := make(chan error, 1)
	defer func() {
		server.Shutdown(context.TODO())
		<-serverDown
	}()

	go func() {
		serverDown <- server.Serve(ln)
		close(serverDown)
	}()

	var options Options

	for {
		select {
		case data := <-ctx.Events:
			if new, ok := data.(Options); ok {
				old := options
				options = new
				if !new.ProxyList.Equals(old.ProxyList) {
					d, _ := new.ProxyList.NewDialer(direct)
					handler.SetDial(d.Dial)
				}
			}
		case err := <-serverDown:
			log.Printf("[http] %v\n", err)
			return
		case <-ctx.Done:
			return
		}
	}
}

func (Service) UnmarshalOptions(text []byte) (interface{}, error) {
	var options Options
	if err := yaml.UnmarshalStrict(text, &options); err != nil {
		return nil, err
	}
	return options, nil
}

type Handler struct {
	tr   *http.Transport
	dial atomic.Value
}

func NewHandler() *Handler {
	h := &Handler{}
	h.dial.Store(direct.Dial)

	dial := func(network, addr string) (net.Conn, error) {
		dial := h.dial.Load().(func(network, addr string) (net.Conn, error))
		return dial(network, addr)
	}

	h.tr = &http.Transport{
		Dial:                  dial,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       10 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return h
}

func (h *Handler) CloseIdleConnections() {
	h.tr.CloseIdleConnections()
}

func (h *Handler) SetDial(dial func(network, addr string) (net.Conn, error)) {
	h.dial.Store(dial)
}

func (h *Handler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodConnect {
		if _, ok := rw.(http.Hijacker); !ok {
			http.Error(rw, "http.ResponseWriter does not implement http.Hijacker.", http.StatusInternalServerError)
			return
		}
		conn, _, err := rw.(http.Hijacker).Hijack()
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		go func() {
			defer conn.Close()

			remote, err := h.tr.Dial("tcp", req.RequestURI)
			if err != nil {
				if _, err := conn.Write([]byte("HTTP/1.1 503 Service Unavailable\r\n\r\n")); err != nil {
					log.Printf("[http] write: %v\n", err)
				}
				return
			}
			defer remote.Close()

			if _, err := conn.Write([]byte("HTTP/1.1 200 OK\r\n\r\n")); err != nil {
				log.Printf("[http] write: %v\n", err)
				return
			}

			if err := utility.Relay(remote, conn); err != nil {
				log.Printf("[http] relay: %v\n", err)
				return
			}
		}()
		return
	}

	req.Header.Del("Connection")
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Proxy-Authenticate")
	req.Header.Del("Proxy-Authorization")

	resp, err := h.tr.RoundTrip(req)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusServiceUnavailable)
		return
	}

	copyHeader(rw.Header(), resp.Header)
	rw.WriteHeader(resp.StatusCode)
	io.Copy(rw, resp.Body)
	resp.Body.Close()
}

func copyHeader(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

var direct = &net.Dialer{
	Timeout:   configure.Timeout,
	KeepAlive: configure.KeepAlive,
}
