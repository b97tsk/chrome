package main

import (
	"log"
	"net"
	"sync"
	"time"

	"github.com/shadowsocks/go-shadowsocks2/socks"
	"gopkg.in/yaml.v2"
)

type socksSettings struct {
	ProxyList ProxyNameList `yaml:"over"`
}

type socksService struct{}

func (socksService) Run(ctx ServiceCtx) {
	log.Printf("[socks] listening on %v\n", ctx.Name)
	listener, err := net.Listen("tcp", ctx.Name)
	if err != nil {
		log.Printf("[socks] %v\n", err)
		return
	}
	defer log.Printf("[socks] stopped listening on %v\n", ctx.Name)

	var (
		connections = make(map[net.Conn]bool)
		cout        = make(chan net.Conn, 4)
		cwg         sync.WaitGroup
	)
	defer func() {
		now := time.Now()
		for c := range connections {
			c.SetDeadline(now)
		}
		go func() {
			for range cout {
			}
		}()
		cwg.Wait()
		close(cout)
	}()

	var (
		cin = make(chan net.Conn, 4)
		lwg sync.WaitGroup
	)
	defer func() {
		listener.Close()
		go func() {
			for c := range cin {
				c.Close()
			}
		}()
		lwg.Wait()
		close(cin)
	}()

	lwg.Add(1)
	go func() {
		defer lwg.Done()
		for {
			c, err := listener.Accept()
			if err != nil {
				if isTemporary(err) {
					continue
				}
				return
			}
			tcpKeepAlive(c, direct.KeepAlive)
			cin <- c
		}
	}()

	var (
		dial      = direct.Dial
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
				dial = d.Dial
			}
		case c := <-cin:
			connections[c] = true

			dial := dial

			cwg.Add(1)
			go func() {
				defer func() {
					c.Close()
					cout <- c
					cwg.Done()
				}()

				addr, err := socks.Handshake(c)
				if err != nil {
					log.Printf("[socks] socks handshake: %v\n", err)
					return
				}

				rc, err := dial("tcp", addr.String())
				if err != nil {
					log.Printf("[socks] %v\n", err)
					return
				}
				defer rc.Close()

				_, _, err = relay(rc, c)
				if err != nil && !isTimeout(err) {
					log.Printf("[socks] relay: %v\n", err)
				}
			}()
		case c := <-cout:
			delete(connections, c)
		case <-ctx.Done:
			return
		}
	}
}

func init() {
	services.Add("socks", socksService{})
}
