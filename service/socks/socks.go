package socks

import (
	"log"
	"net"
	"sync/atomic"

	"github.com/b97tsk/chrome/configure"
	"github.com/b97tsk/chrome/internal/utility"
	"github.com/b97tsk/chrome/service"
	"github.com/shadowsocks/go-shadowsocks2/socks"
	"gopkg.in/yaml.v2"
)

type Options struct {
	ProxyList service.ProxyList `yaml:"over"`
}

type Service struct{}

func (Service) Name() string {
	return "socks"
}

func (Service) Aliases() []string {
	return []string{"socks5"}
}

func (Service) Run(ctx service.Context) {
	ln, err := net.Listen("tcp", ctx.ListenAddr)
	if err != nil {
		log.Printf("[socks] %v\n", err)
		return
	}
	log.Printf("[socks] listening on %v\n", ln.Addr())
	defer log.Printf("[socks] stopped listening on %v\n", ln.Addr())
	defer ln.Close()

	var dial atomic.Value
	dial.Store(direct.Dial)

	ctx.Manager.ServeListener(ln, func(c net.Conn) {
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

		if err = utility.Relay(rc, c); err != nil {
			log.Printf("[socks] relay: %v\n", err)
		}
	})

	var (
		options Options
	)
	for {
		select {
		case data := <-ctx.Events:
			if new, ok := data.(Options); ok {
				old := options
				options = new
				if !new.ProxyList.Equals(old.ProxyList) {
					d, _ := new.ProxyList.NewDialer(direct)
					dial.Store(d.Dial)
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
