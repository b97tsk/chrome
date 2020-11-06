package service

import (
	"net"
	"sync"
)

func (man *Manager) ServeListener(ln net.Listener, handle func(net.Conn)) {
	man.serveListener(ln, handle)
}

type servingService struct {
	connections sync.Map
}

func (s *servingService) serveListener(ln net.Listener, handle func(net.Conn)) {
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				if isTemporary(err) {
					continue
				}
				return
			}
			s.connections.Store(c, struct{}{})
			go func() {
				defer func() {
					c.Close()
					s.connections.Delete(c)
				}()
				handle(c)
			}()
		}
	}()
}

func (s *servingService) closeConnections() {
	s.connections.Range(func(key, _ interface{}) bool {
		key.(net.Conn).Close()
		return true
	})
}
