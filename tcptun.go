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

		cin := make(chan net.Conn, 4)
		defer func() {
			for c := range cin {
				c.Close()
			}
		}()

		var (
			activeListener net.Listener
			lnwg           sync.WaitGroup
		)
		defer func() {
			if activeListener != nil {
				// log.Printf("[%v] closing %v\n", job.name, activeListener.Addr())
				activeListener.Close()
				lnwg.Wait()
			}
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
						if activeListener != nil {
							// log.Printf("[%v] closing %v\n", job.name, activeListener.Addr())
							activeListener.Close()
							activeListener = nil
						}
						log.Printf("[%v] listening on %v\n", job.name, settings.ListenAddr)
						ln, err := net.Listen("tcp", settings.ListenAddr)
						if err != nil {
							log.Printf("[%v] %v\n", job.name, err)
						} else {
							activeListener = ln
							lnwg.Add(1)
							go func() {
								defer lnwg.Done()
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

				forwardAddr := settings.ForwardAddr
				cwg.Add(1)
				go func() {
					defer cwg.Done()
					defer func() {
						c.Close()
						cout <- c
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
