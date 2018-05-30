package main

import (
	"log"
	"net"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v2"
)

type tcptunOptions struct {
	ForwardAddr string        `yaml:"for"`
	ProxyList   ProxyNameList `yaml:"over"`
}

type tcptunService struct{}

func (tcptunService) Run(ctx ServiceCtx) {
	ln, err := net.Listen("tcp", ctx.Name)
	if err != nil {
		log.Printf("[tcptun] %v\n", err)
		return
	}
	log.Printf("[tcptun] listening on %v\n", ln.Addr())
	defer log.Printf("[tcptun] stopped listening on %v\n", ln.Addr())

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
		options   tcptunOptions
		proxyList ProxyList
	)
	for {
		select {
		case data := <-ctx.Events:
			if new, ok := data.(tcptunOptions); ok {
				old := options
				options = new
				shouldUpdate := new.ForwardAddr != old.ForwardAddr
				if pl := services.ProxyList(new.ProxyList...); !pl.Equals(proxyList) {
					proxyList = pl
					d, _ := proxyList.Dialer(direct)
					dial = d.Dial
					shouldUpdate = true
				}
				if shouldUpdate {
					dial := dial
					addr := new.ForwardAddr
					connect.Store(func() (net.Conn, error) { return dial("tcp", addr) })
				}
			}
		case <-ctx.Done:
			return
		}
	}
}

func (tcptunService) UnmarshalOptions(text []byte) (interface{}, error) {
	var options tcptunOptions
	if err := yaml.UnmarshalStrict(text, &options); err != nil {
		return nil, err
	}
	return options, nil
}

func init() {
	services.Add("tcptun", tcptunService{})
}
