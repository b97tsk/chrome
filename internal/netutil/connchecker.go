package netutil

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

type connChecker struct {
	net.Conn

	cbr      chan *bufio.Reader
	beat     chan struct{}
	cancel   func()
	deadline chan time.Time
}

// NewConnChecker returns a new net.Conn based on conn, and a context.Context
// whose cancellation indicates that conn is no longer readable (closed or
// lost).
//
// Internally, NewConnChecker starts a goroutine that periodically checks
// if conn is no longer readable (closed or lost); if yes, it cancels the
// associated context.Context.
func NewConnChecker(conn net.Conn) (net.Conn, context.Context) {
	ctx, cancel := context.WithCancel(context.Background())

	c := &connChecker{
		Conn:     conn,
		cbr:      make(chan *bufio.Reader, 1),
		beat:     make(chan struct{}, 1),
		cancel:   cancel,
		deadline: make(chan time.Time, 1),
	}

	c.cbr <- bufio.NewReaderSize(conn, checkBufferSize)
	c.deadline <- time.Time{}

	go c.start(ctx)

	return c, ctx
}

func (c *connChecker) start(ctx context.Context) {
	defer c.cancel()

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
					deadline := <-c.deadline
					_ = c.Conn.SetReadDeadline(time.Now().Add(checkWindow))
					_, err = br.Peek(checkBufferSize)
					_ = c.Conn.SetReadDeadline(deadline)
					c.deadline <- deadline
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

func (c *connChecker) Read(p []byte) (n int, err error) {
	br := <-c.cbr
	n, err = br.Read(p)
	c.cbr <- br

	if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		c.cancel()
		return
	}

	select {
	case c.beat <- struct{}{}:
	default:
	}

	return
}

func (c *connChecker) Close() error {
	c.cancel()

	return c.Conn.Close()
}

func (c *connChecker) SetDeadline(t time.Time) error {
	<-c.deadline
	err := c.Conn.SetDeadline(t)
	c.deadline <- t

	return err
}

func (c *connChecker) SetReadDeadline(t time.Time) error {
	<-c.deadline
	err := c.Conn.SetReadDeadline(t)
	c.deadline <- t

	return err
}
