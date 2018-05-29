package main

import (
	"log"
	"net"
	"sync"
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

	var wg sync.WaitGroup
	defer wg.Wait()
	defer ln.Close()

	wg.Add(1)
	go func() {
		var connections struct {
			sync.Map
			sync.WaitGroup
		}
		defer func() {
			connections.Range(func(key, _ interface{}) bool {
				key.(net.Conn).Close()
				return true
			})
			connections.Wait()
			wg.Done()
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				if isTemporary(err) {
					continue
				}
				return
			}
			tcpKeepAlive(c, direct.KeepAlive)
			connections.Store(c, struct{}{})
			connections.Add(1)
			go func() {
				defer connections.Done()
				defer connections.Delete(c)
				defer c.Close()

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
			}()
		}
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
