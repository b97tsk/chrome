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

type ConnChecker struct {
	net.Conn

	cbr      chan *bufio.Reader
	ping     chan struct{}
	cancel   func()
	deadline time.Time
}

func NewConnChecker(conn net.Conn) (*ConnChecker, context.Context) {
	ctx, cancel := context.WithCancel(context.Background())
	cbr := make(chan *bufio.Reader, 1)
	cbr <- bufio.NewReaderSize(conn, checkBufferSize)
	ping := make(chan struct{}, 1)
	c := &ConnChecker{
		Conn:   conn,
		cbr:    cbr,
		ping:   ping,
		cancel: cancel,
	}
	go c.start(ctx)
	return c, ctx
}

func (c *ConnChecker) start(ctx context.Context) {
	defer c.cancel()
	var skipNextCheck bool
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	done := ctx.Done()
	for {
		select {
		case <-done:
			return
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
					c.Conn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
					_, err = br.Peek(checkBufferSize)
					c.Conn.SetReadDeadline(c.deadline)
				}
				c.cbr <- br
				if err != nil && err != bufio.ErrBufferFull && !os.IsTimeout(err) {
					return
				}
			default:
			}
		}
	}
}

func (c *ConnChecker) Read(p []byte) (n int, err error) {
	br := <-c.cbr
	n, err = br.Read(p)
	c.cbr <- br
	select {
	case c.ping <- struct{}{}:
	default:
	}
	return
}

func (c *ConnChecker) Close() error {
	c.cancel()
	return c.Conn.Close()
}

func (c *ConnChecker) SetDeadline(t time.Time) error {
	br := <-c.cbr
	err := c.Conn.SetDeadline(t)
	c.deadline = t
	c.cbr <- br
	return err
}

func (c *ConnChecker) SetReadDeadline(t time.Time) error {
	br := <-c.cbr
	err := c.Conn.SetReadDeadline(t)
	c.deadline = t
	c.cbr <- br
	return err
}
