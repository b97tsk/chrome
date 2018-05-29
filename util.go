package main

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"sync"
	"time"
)

func tcpKeepAlive(c net.Conn, d time.Duration) {
	if tcp, ok := c.(*net.TCPConn); ok && d > 0 {
		tcp.SetKeepAlive(true)
		tcp.SetKeepAlivePeriod(d)
	}
}

func decodeBase64String(s string) ([]byte, error) {
	enc := base64.StdEncoding
	if len(s)%4 != 0 {
		enc = base64.RawStdEncoding
	}
	return enc.DecodeString(s)
}

func isTemporary(err error) bool {
	e, ok := err.(interface {
		Temporary() bool
	})
	return ok && e.Temporary()
}

func isTimeout(err error) bool {
	e, ok := err.(interface {
		Timeout() bool
	})
	return ok && e.Timeout()
}

func firstError(errors ...error) error {
	for _, err := range errors {
		if err != nil {
			return err
		}
	}
	return nil
}

func closeRead(c net.Conn) {
	if cr, ok := c.(interface {
		CloseRead() error
	}); ok {
		cr.CloseRead()
	}
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface {
		CloseWrite() error
	}); ok {
		cw.CloseWrite()
	}
}

func relay(left, right net.Conn) error {
	wait := make(chan error, 1)
	go func() { wait <- relayCopy(right, left) }()
	err := relayCopy(left, right)
	return firstError(err, <-wait)
}

func relayCopy(dst, src net.Conn) error {
	_, err := io.Copy(dst, src)
	closeRead(src)
	closeWrite(dst)
	return err
}

func serve(ln net.Listener, handle func(net.Conn)) context.Context {
	ctx, cancel := context.WithCancel(context.TODO())
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
			cancel()
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
				defer func() {
					c.Close()
					connections.Delete(c)
					connections.Done()
				}()
				handle(c)
			}()
		}
	}()
	return ctx
}