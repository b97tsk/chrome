package main

import (
	"log"
	"net"
	"sync"
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
	listener, err := net.Listen("tcp", ctx.Name)
	if err != nil {
		log.Printf("[tcptun] %v\n", err)
		return
	}
	defer log.Printf("[tcptun] stopped listening on %v\n", ctx.Name)

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
			if pl := services.ProxyList(settings.ProxyList...); !pl.Equals(proxyList) {
				proxyList = pl
				d, _ := proxyList.Dialer(direct)
				dial = d.Dial
			}
		case c := <-cin:
			connections[c] = true

			dial := dial
			forwardAddr := settings.ForwardAddr

			cwg.Add(1)
			go func() {
				defer func() {
					c.Close()
					cout <- c
					cwg.Done()
				}()

				rc, err := dial("tcp", forwardAddr)
				if err != nil {
					log.Printf("[tcptun] %v\n", err)
					return
				}
				defer rc.Close()

				_, _, err = relay(rc, c)
				if err != nil && !isTimeout(err) {
					log.Printf("[tcptun] relay: %v\n", err)
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
	services.Add("tcptun", tcptunService{})
}
