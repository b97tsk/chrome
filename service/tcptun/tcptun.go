package tcptun

import (
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/b97tsk/chrome/configure"
	"github.com/b97tsk/chrome/internal/proxy"
	"github.com/b97tsk/chrome/internal/utility"
	"gopkg.in/yaml.v2"
)

type Options struct {
	ForwardAddr string           `yaml:"for"`
	Proxy       proxy.ProxyChain `yaml:"over"`
}

type Service struct{}

func (Service) Name() string {
	return "tcptun"
}

func (Service) Run(ctx service.Context) {
	ln, err := net.Listen("tcp", ctx.ListenAddr)
	if err != nil {
		log.Printf("[tcptun] %v\n", err)
		return
	}
	log.Printf("[tcptun] listening on %v\n", ln.Addr())
	defer log.Printf("[tcptun] stopped listening on %v\n", ln.Addr())
	defer ln.Close()

	var connect atomic.Value

	ctx.Manager.ServeListener(ln, func(c net.Conn) {
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
			// log.Printf("[tcptun] %v\n", err)
			return
		}
		defer rc.Close()

		utility.Relay(rc, c)
	})

	var (
		dial    = direct.Dial
		options Options
	)
	for {
		select {
		case data := <-ctx.Events:
			if new, ok := data.(Options); ok {
				old := options
				options = new
				shouldUpdate := new.ForwardAddr != old.ForwardAddr
				if !new.Proxy.Equals(old.Proxy) {
					d, _ := new.Proxy.NewDialer(direct)
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

func (Service) UnmarshalOptions(text []byte) (interface{}, error) {
	var options Options
	if err := yaml.UnmarshalStrict(text, &options); err != nil {
		return nil, err
	}
	return options, nil
}

var direct = &net.Dialer{
	Timeout:   configure.Timeout,
	KeepAlive: configure.KeepAlive,
}
