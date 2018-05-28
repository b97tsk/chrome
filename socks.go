package main

import (
	"context"
	"log"
	"net"
	"sync"
	"time"

	"github.com/shadowsocks/go-shadowsocks2/socks"
	"gopkg.in/yaml.v2"
)

type socksSettings struct {
	ListenAddr string        `yaml:"listen"`
	ProxyList  ProxyNameList `yaml:"over"`
}

type socksJob struct {
	name   string
	event  chan interface{}
	done   chan struct{}
	cancel context.CancelFunc
}

func (job *socksJob) start() {
	// log.Printf("[%v] started\n", job.name)

	ctx, cancel := context.WithCancel(context.TODO())
	job.cancel = cancel

	go func() {
		defer close(job.done)
		defer log.Printf("[%v] stopped\n", job.name)

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
			cin      = make(chan net.Conn, 4)
			lwg      sync.WaitGroup
			listener net.Listener
		)
		defer func() {
			if listener != nil {
				// log.Printf("[%v] closing %v\n", job.name, listener.Addr())
				listener.Close()
				listener = nil
			}
			go func() {
				for c := range cin {
					c.Close()
				}
			}()
			lwg.Wait()
			close(cin)
		}()

		var (
			settings  socksSettings
			proxyList ProxyList
			dial      = direct.Dial
		)

		for {
			select {
			case v := <-job.event:
				if s, ok := v.(socksSettings); ok {
					settings, s = s, settings
					if settings.ListenAddr != s.ListenAddr {
						if listener != nil {
							// log.Printf("[%v] closing %v\n", job.name, listener.Addr())
							listener.Close()
							listener = nil
						}
						log.Printf("[%v] listening on %v\n", job.name, settings.ListenAddr)
						ln, err := net.Listen("tcp", settings.ListenAddr)
						if err != nil {
							log.Printf("[%v] %v\n", job.name, err)
						} else {
							listener = ln
							lwg.Add(1)
							go func() {
								defer lwg.Done()
								for {
									c, err := ln.Accept()
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
						}
					}
					if pl := services.ProxyList(settings.ProxyList...); !pl.Equals(proxyList) {
						proxyList = pl
						d, _ := proxyList.Dialer(direct)
						dial = d.Dial
					}
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
						log.Printf("[%v] socks handshake: %v\n", job.name, err)
						return
					}

					rc, err := dial("tcp", addr.String())
					if err != nil {
						log.Printf("[%v] %v\n", job.name, err)
						return
					}
					defer rc.Close()

					_, _, err = relay(rc, c)
					if err != nil && !isTimeout(err) {
						log.Printf("[%v] relay: %v\n", job.name, err)
					}
				}()
			case c := <-cout:
				delete(connections, c)
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (job *socksJob) Send(v interface{}) {
	values := []interface{}{v, nil}
	for _, v := range values {
		select {
		case job.event <- v:
		case <-job.done:
			return
		}
	}
}

func (job *socksJob) Stop() {
	job.cancel()
}

func (job *socksJob) Done() <-chan struct{} {
	return job.done
}

type socksService struct{}

func (socksService) UnmarshalSettings(data []byte) (interface{}, error) {
	var settings socksSettings
	if err := yaml.UnmarshalStrict(data, &settings); err != nil {
		return nil, err
	}
	return settings, nil
}

func (socksService) StartNewJob(name string) Job {
	job := socksJob{
		name:  name,
		event: make(chan interface{}),
		done:  make(chan struct{}),
	}
	job.start()
	return &job
}

func init() {
	services.Add("socks", socksService{})
}
