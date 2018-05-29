package main

import (
	"log"
	"net"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v2"
)

type tcptunSettings struct {
	ForwardAddr string        `yaml:"for"`
	ProxyList   ProxyNameList `yaml:"over"`
}

type tcptunService struct{}

func (tcptunService) Run(ctx ServiceCtx) {
	log.Printf("[tcptun] listening on %v\n", ctx.Name)
	ln, err := net.Listen("tcp", ctx.Name)
	if err != nil {
		log.Printf("[tcptun] %v\n", err)
		return
	}
	defer log.Printf("[tcptun] stopped listening on %v\n", ctx.Name)

	var connect atomic.Value

	serving := serve(ln, func(c net.Conn) {
		connectLoad := connect.Load
		connect := connectLoad()
		if connect == nil {
			time.Sleep(time.Second)
			connect = connectLoad()
			if connect == nil {
				return
			}
		}

		rc, err := connect.(func() (net.Conn, error))()
		if err != nil {
			log.Printf("[tcptun] %v\n", err)
			return
		}
		defer rc.Close()

		if err = relay(rc, c); err != nil && !isTimeout(err) {
			log.Printf("[tcptun] relay: %v\n", err)
		}
	})
	defer func() {
		ln.Close()
		<-serving.Done()
	}()

	var (
		dial      = direct.Dial
		settings  tcptunSettings
		proxyList ProxyList
	)
	for {
		select {
		case data := <-ctx.Events:
			if data == nil {
				continue
			}
			var s tcptunSettings
			bytes, _ := yaml.Marshal(data)
			if err := yaml.UnmarshalStrict(bytes, &s); err != nil {
				log.Printf("[tcptun] unmarshal: %v\n", err)
				continue
			}
			settings, s = s, settings
			shouldUpdate := settings.ForwardAddr != s.ForwardAddr
			if pl := services.ProxyList(settings.ProxyList...); !pl.Equals(proxyList) {
				proxyList = pl
				d, _ := proxyList.Dialer(direct)
				dial = d.Dial
				shouldUpdate = true
			}
			if shouldUpdate {
				dial := dial
				addr := settings.ForwardAddr
				connect.Store(func() (net.Conn, error) { return dial("tcp", addr) })
			}
		case <-ctx.Done:
			return
		}
	}
}

func init() {
	services.Add("tcptun", tcptunService{})
}
