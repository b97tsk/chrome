package service

import (
	"bufio"
	"context"
	"net"
	"os"
	"time"
)

const (
	checkInterval   = 1 * time.Second
	checkBufferSize = 4096
)

type bufconn struct {
	net.Conn
	cbr      chan *bufio.Reader
	ping     chan struct{}
	cancel   func()
	deadline time.Time
}

func (c *bufconn) Read(p []byte) (n int, err error) {
	br := <-c.cbr
	n, err = br.Read(p)
	c.cbr <- br
	select {
	case c.ping <- struct{}{}:
	default:
	}
	return
}

func (c *bufconn) Close() error {
	c.cancel()
	return c.Conn.Close()
}

func (c *bufconn) SetDeadline(t time.Time) error {
	br := <-c.cbr
	err := c.Conn.SetDeadline(t)
	c.deadline = t
	c.cbr <- br
	return err
}

func (c *bufconn) SetReadDeadline(t time.Time) error {
	br := <-c.cbr
	err := c.Conn.SetReadDeadline(t)
	c.deadline = t
	c.cbr <- br
	return err
}

func CheckConnectivity(ctx context.Context, conn net.Conn) (context.Context, net.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	cbr := make(chan *bufio.Reader, 1)
	cbr <- bufio.NewReaderSize(conn, checkBufferSize)
	ping := make(chan struct{}, 1)
	c := &bufconn{Conn: conn, cbr: cbr, ping: ping, cancel: cancel}
	go func() {
		defer cancel()
		var skipNextCheck bool
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()
		done := ctx.Done()
		for {
			select {
			case <-c.ping:
				skipNextCheck = true
			case <-ticker.C:
				if skipNextCheck {
					skipNextCheck = false
					continue
				}
				select {
				case br := <-c.cbr:
					var err error
					if br.Buffered() < checkBufferSize {
						conn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
						_, err = br.Peek(checkBufferSize)
						conn.SetReadDeadline(c.deadline)
					}
					c.cbr <- br
					if err != nil && err != bufio.ErrBufferFull && !os.IsTimeout(err) {
						return
					}
				default:
				}
			case <-done:
				return
			}
		}
	}()
	return ctx, c
}
