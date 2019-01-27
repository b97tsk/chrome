package main

import (
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/b97tsk/chrome/internal/proxy"
	"github.com/b97tsk/chrome/internal/utility"
	"github.com/b97tsk/chrome/service"
	"gopkg.in/yaml.v2"
)

type tcptunOptions struct {
	ForwardAddr string          `yaml:"for"`
	ProxyList   proxy.ProxyList `yaml:"over"`
}

type tcptunService struct{}

func (tcptunService) Name() string {
	return "tcptun"
}

func (tcptunService) Run(ctx service.Context) {
	ln, err := net.Listen("tcp", ctx.ListenAddr)
	if err != nil {
		log.Printf("[tcptun] %v\n", err)
		return
	}
	log.Printf("[tcptun] listening on %v\n", ln.Addr())
	defer log.Printf("[tcptun] stopped listening on %v\n", ln.Addr())
	defer ln.Close()

	var connect atomic.Value

	services.ServeListener(ln, func(c net.Conn) {
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

		if err = utility.Relay(rc, c); err != nil {
			log.Printf("[tcptun] relay: %v\n", err)
		}
	})

	var (
		dial    = direct.Dial
		options tcptunOptions
	)
	for {
		select {
		case data := <-ctx.Events:
			if new, ok := data.(tcptunOptions); ok {
				old := options
				options = new
				shouldUpdate := new.ForwardAddr != old.ForwardAddr
				if !new.ProxyList.Equals(old.ProxyList) {
					d, _ := new.ProxyList.NewDialer(direct)
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
