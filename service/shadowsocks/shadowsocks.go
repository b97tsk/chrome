package shadowsocks

import (
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/b97tsk/chrome/configure"
	"github.com/b97tsk/chrome/internal/proxy"
	"github.com/b97tsk/chrome/internal/utility"
	"github.com/b97tsk/chrome/service"
	"github.com/shadowsocks/go-shadowsocks2/core"
	"github.com/shadowsocks/go-shadowsocks2/socks"
	"gopkg.in/yaml.v2"
)

type Options struct {
	Method    string
	Password  string
	ProxyList proxy.ProxyList `yaml:"over"`
}

type Service struct{}

func (Service) Name() string {
	return "shadowsocks"
}

func (Service) Run(ctx service.Context) {
	ln, err := net.Listen("tcp", ctx.ListenAddr)
	if err != nil {
		log.Printf("[shadowsocks] %v\n", err)
		return
	}
	log.Printf("[shadowsocks] listening on %v\n", ln.Addr())
	defer log.Printf("[shadowsocks] stopped listening on %v\n", ln.Addr())
	defer ln.Close()

	type Cipher struct {
		core.Cipher
	}

	var (
		dial   atomic.Value
		cipher atomic.Value
	)
	dial.Store(direct.Dial)

	ctx.Manager.ServeListener(ln, func(c net.Conn) {
		cipherLoad := cipher.Load
		cipher := cipherLoad()
		if cipher == nil {
			time.Sleep(time.Second)
			cipher = cipherLoad()
			if cipher == nil {
				return
			}
		}

		c = cipher.(Cipher).StreamConn(c)
		addr, err := socks.ReadAddr(c)
		if err != nil {
			log.Printf("[shadowsocks] read addr: %v\n", err)
			return
		}

		dial := dial.Load().(func(network, addr string) (net.Conn, error))
		rc, err := dial("tcp", addr.String())
		if err != nil {
			log.Printf("[shadowsocks] %v\n", err)
			return
		}
		defer rc.Close()

		if err = utility.Relay(rc, c); err != nil {
			log.Printf("[shadowsocks] relay: %v\n", err)
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
				if new.Method != old.Method || new.Password != old.Password {
					if c, err := core.PickCipher(new.Method, nil, new.Password); err != nil {
						log.Printf("[shadowsocks] pick cipher: %v\n", err)
					} else {
						cipher.Store(Cipher{c})
					}
				}
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
