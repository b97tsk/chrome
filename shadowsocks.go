package main

import (
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/shadowsocks/go-shadowsocks2/core"
	"github.com/shadowsocks/go-shadowsocks2/socks"
	"gopkg.in/yaml.v2"
)

type shadowsocksOptions struct {
	Method    string
	Password  string
	ProxyList ProxyNameList `yaml:"over"`
}

type shadowsocksService struct{}

func (shadowsocksService) Run(ctx ServiceCtx) {
	ln, err := net.Listen("tcp", ctx.Name)
	if err != nil {
		log.Printf("[shadowsocks] %v\n", err)
		return
	}
	log.Printf("[shadowsocks] listening on %v\n", ln.Addr())
	defer log.Printf("[shadowsocks] stopped listening on %v\n", ln.Addr())

	var (
		dial   atomic.Value
		cipher atomic.Value
	)
	dial.Store(direct.Dial)

	serving := serve(ln, func(c net.Conn) {
		cipherLoad := cipher.Load
		cipher := cipherLoad()
		if cipher == nil {
			time.Sleep(time.Second)
			cipher = cipherLoad()
			if cipher == nil {
				return
			}
		}

		c = cipher.(core.Cipher).StreamConn(c)
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

		if err = relay(rc, c); err != nil && !isTimeout(err) {
			log.Printf("[shadowsocks] relay: %v\n", err)
		}
	})
	defer func() {
		ln.Close()
		<-serving.Done()
	}()

	var (
		options   shadowsocksOptions
		proxyList ProxyList
	)
	for {
		select {
		case data := <-ctx.Events:
			if new, ok := data.(shadowsocksOptions); ok {
				old := options
				options = new
				if new.Method != old.Method || new.Password != old.Password {
					if c, err := core.PickCipher(new.Method, nil, new.Password); err != nil {
						log.Printf("[shadowsocks] pick cipher: %v\n", err)
					} else {
						cipher.Store(c)
					}
				}
				if pl := services.ProxyList(new.ProxyList...); !pl.Equals(proxyList) {
					proxyList = pl
					d, _ := proxyList.Dialer(direct)
					dial.Store(d.Dial)
				}
			}
		case <-ctx.Done:
			return
		}
	}
}

func (shadowsocksService) UnmarshalOptions(text []byte) (interface{}, error) {
	var options shadowsocksOptions
	if err := yaml.UnmarshalStrict(text, &options); err != nil {
		return nil, err
	}
	return options, nil
}

func init() {
	services.Add("shadowsocks", shadowsocksService{})
}