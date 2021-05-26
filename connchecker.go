package chrome

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"time"
)

const (
	checkInterval   = 500 * time.Millisecond
	checkWindow     = 1 * time.Millisecond
	checkBufferSize = 1024
)

type ConnChecker struct {
	net.Conn

	cbr      chan *bufio.Reader
	beat     chan struct{}
	cancel   func()
	deadline time.Time
}

func NewConnChecker(conn net.Conn) (*ConnChecker, context.Context) {
	return NewConnCheckerContext(context.Background(), conn)
}

func NewConnCheckerContext(ctx context.Context, conn net.Conn) (*ConnChecker, context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	cbr := make(chan *bufio.Reader, 1)
	cbr <- bufio.NewReaderSize(conn, checkBufferSize)

	beat := make(chan struct{}, 1)
	c := &ConnChecker{
		Conn:   conn,
		cbr:    cbr,
		beat:   beat,
		cancel: cancel,
	}

	go c.start(ctx)

	return c, ctx
}

func (c *ConnChecker) start(ctx context.Context) {
	defer c.Close()

	var skipNextCheck bool

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	done := ctx.Done()

	for {
		select {
		case <-done:
			return
		case <-c.beat:
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
					_ = c.Conn.SetReadDeadline(time.Now().Add(checkWindow))
					_, err = br.Peek(checkBufferSize)
					_ = c.Conn.SetReadDeadline(c.deadline)
				}

				c.cbr <- br

				switch {
				case err == nil:
				case err == bufio.ErrBufferFull:
				case errors.Is(err, os.ErrDeadlineExceeded):
				default:
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
	case c.beat <- struct{}{}:
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
