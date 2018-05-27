package main

import (
	"context"
	"log"
	"net"
	"sync"
	"time"

	"gopkg.in/yaml.v2"
)

const tcptunTypeName = "tcptun"

type tcptunSettings struct {
	Type        string
	ListenAddr  string        `yaml:"listen"`
	ForwardAddr string        `yaml:"forward"`
	ProxyList   ProxyNameList `yaml:"over"`
}

type tcptunJob struct {
	name   string
	event  chan interface{}
	done   chan struct{}
	cancel context.CancelFunc
}

func (job *tcptunJob) start() {
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
			settings  tcptunSettings
			proxyList ProxyList
			dial      = direct.Dial
		)

		for {
			select {
			case v := <-job.event:
				if s, ok := v.(tcptunSettings); ok {
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

func (*tcptunJob) Type() string {
	return tcptunTypeName
}

func (job *tcptunJob) Send(v interface{}) {
	values := []interface{}{v, nil}
	for _, v := range values {
		select {
		case job.event <- v:
		case <-job.done:
			return
		}
	}
}

func (job *tcptunJob) Stop() {
	job.cancel()
}

func (job *tcptunJob) Done() <-chan struct{} {
	return job.done
}

type tcptunService struct{}

func (tcptunService) Type() string {
	return tcptunTypeName
}

func (tcptunService) UnmarshalSettings(data []byte) (interface{}, error) {
	var settings tcptunSettings
	if err := yaml.UnmarshalStrict(data, &settings); err != nil {
		return nil, err
	}
	return settings, nil
}

func (tcptunService) StartNewJob(name string) Job {
	job := tcptunJob{
		name:  name,
		event: make(chan interface{}),
		done:  make(chan struct{}),
	}
	job.start()
	return &job
}

func init() {
	services.Add(tcptunService{})
}
