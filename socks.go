package main

import (
	"log"
	"net"
	"sync/atomic"

	"github.com/shadowsocks/go-shadowsocks2/socks"
	"gopkg.in/yaml.v2"
)

type socksSettings struct {
	ProxyList ProxyNameList `yaml:"over"`
}

type socksService struct{}

func (socksService) Run(ctx ServiceCtx) {
	log.Printf("[socks] listening on %v\n", ctx.Name)
	ln, err := net.Listen("tcp", ctx.Name)
	if err != nil {
		log.Printf("[socks] %v\n", err)
		return
	}
	defer log.Printf("[socks] stopped listening on %v\n", ctx.Name)

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
		settings  socksSettings
		proxyList ProxyList
	)
	for {
		select {
		case data := <-ctx.Events:
			if data == nil {
				continue
			}
			var s socksSettings
			bytes, _ := yaml.Marshal(data)
			if err := yaml.UnmarshalStrict(bytes, &s); err != nil {
				log.Printf("[socks] unmarshal: %v\n", err)
				continue
			}
			settings, s = s, settings
			if pl := services.ProxyList(settings.ProxyList...); !pl.Equals(proxyList) {
				proxyList = pl
				d, _ := proxyList.Dialer(direct)
				dial.Store(d.Dial)
			}
		case <-ctx.Done:
			return
		}
	}
}

func init() {
	services.Add("socks", socksService{})
}
