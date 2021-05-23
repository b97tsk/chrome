package chrome

import (
	"net"
	"sync"
	"time"
)

type servingService struct {
	connections sync.Map
}

func (m *servingService) Serve(ln net.Listener, fn func(net.Conn)) {
	var tempDelay time.Duration // how long to sleep on accept failure

	for {
		c, err := ln.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}

				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}

				time.Sleep(tempDelay)

				continue
			}

			return
		}

		tempDelay = 0

		m.connections.Store(c, struct{}{})

		go func() {
			defer func() {
				c.Close()
				m.connections.Delete(c)
			}()
			fn(c)
		}()
	}
}

func (m *servingService) CloseConnections() {
	m.connections.Range(func(key, _ interface{}) bool {
		_ = key.(net.Conn).Close()
		return true
	})
}
