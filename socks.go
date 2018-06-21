package main

import (
	"log"
	"net"
	"sync/atomic"

	"github.com/shadowsocks/go-shadowsocks2/socks"
	"gopkg.in/yaml.v2"
)

type socksOptions struct {
	ProxyList ProxyList `yaml:"over"`
}

type socksService struct{}

func (socksService) Run(ctx ServiceCtx) {
	ln, err := net.Listen("tcp", ctx.Name)
	if err != nil {
		log.Printf("[socks] %v\n", err)
		return
	}
	log.Printf("[socks] listening on %v\n", ln.Addr())
	defer log.Printf("[socks] stopped listening on %v\n", ln.Addr())

	var dial atomic.Value
	dial.Store(direct.Dial)

	serving := serve(ln, func(c net.Conn) {
		addr, err := socks.Handshake(c)
		if err != nil {
			log.Printf("[socks] socks handshake: %v\n", err)
			return
		}

		dial := dial.Load().(func(network, addr string) (net.Conn, error))
		rc, err := dial("tcp", addr.String())
		if err != nil {
			log.Printf("[socks] %v\n", err)
			return
		}
		defer rc.Close()

		if err = relay(rc, c); err != nil && !isTimeout(err) {
			log.Printf("[socks] relay: %v\n", err)
		}
	})
	defer func() {
		ln.Close()
		<-serving.Done()
	}()

	var (
		options socksOptions
	)
	for {
		select {
		case data := <-ctx.Events:
			if new, ok := data.(socksOptions); ok {
				old := options
				options = new
				if !new.ProxyList.Equals(old.ProxyList) {
					d, _ := new.ProxyList.Dialer(direct)
					dial.Store(d.Dial)
				}
			}
		case <-ctx.Done:
			return
		}
	}
}

func (socksService) UnmarshalOptions(text []byte) (interface{}, error) {
	var options socksOptions
	if err := yaml.UnmarshalStrict(text, &options); err != nil {
		return nil, err
	}
	return options, nil
}

func init() {
	services.Add("socks", socksService{})
}
