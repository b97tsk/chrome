package netutil

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync/atomic"
	"time"
)

const (
	checkInterval            = 500 * time.Millisecond
	checkDuration            = 1 * time.Millisecond
	checkBufferInitialSize   = 2 * 1024
	checkBufferMaximumSize   = 32 * 1024
	checkBufferGrowThreshold = 10 * checkInterval
)

var (
	ErrFullBuffer = errors.New("full buffer")
	ErrClosed     = errors.New("closed")
)

type ConnChecker struct {
	net.Conn
	context.Context

	cancel   context.CancelCauseFunc
	stop     atomic.Bool
	cbr      chan *bufio.Reader
	deadline chan time.Time
}

// NewConnChecker creates a ConnChecker cc that periodically checks if c is
// no longer readable (closed or lost); if yes, it cancels cc.Context.
//
// c must not be used afterward (use cc instead).
func NewConnChecker(c net.Conn) (cc *ConnChecker) {
	ctx, cancel := context.WithCancelCause(context.Background())

	cc = &ConnChecker{
		Conn:     c,
		Context:  ctx,
		cancel:   cancel,
		cbr:      make(chan *bufio.Reader, 1),
		deadline: make(chan time.Time, 1),
	}

	cc.cbr <- bufio.NewReaderSize(c, checkBufferInitialSize)
	cc.deadline <- time.Time{}

	go func() { _ = cc.start(ctx) }()

	return
}

func (c *ConnChecker) start(ctx context.Context) (cause error) {
	defer func() {
		if cause != nil {
			c.cancel(cause)
		}
	}()

	var growtime time.Time

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	done := ctx.Done()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if c.stop.Load() {
				return
			}
			select {
			case br := <-c.cbr:
				var err error

				if br.Buffered() < br.Size() {
					deadline := <-c.deadline
					_ = c.Conn.SetReadDeadline(time.Now().Add(checkDuration))
					_, err = br.Peek(br.Size())
					_ = c.Conn.SetReadDeadline(deadline)
					c.deadline <- deadline
					growtime = time.Time{}
				}

				if br.Buffered() == br.Size() {
					if size := br.Size(); size < checkBufferMaximumSize {
						switch now := time.Now(); {
						case growtime.IsZero():
							growtime = now.Add(checkBufferGrowThreshold)
						case now.After(growtime):
							rd := io.MultiReader(io.LimitReader(br, int64(size)), c.Conn)
							br = bufio.NewReaderSize(rd, size*2)
							growtime = time.Time{}
						}
					} else {
						err = ErrFullBuffer
					}
				}

				c.cbr <- br

				if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
					return err
				}
			default:
			}
		}
	}
}

func (c *ConnChecker) Stop() {
	c.stop.Store(true)
}

func (c *ConnChecker) Read(p []byte) (n int, err error) {
	br := <-c.cbr
	n, err = br.Read(p)
	c.cbr <- br

	if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		c.cancel(err)
	}

	return
}

func (c *ConnChecker) Close() error {
	c.cancel(ErrClosed)
	return c.Conn.Close()
}

func (c *ConnChecker) SetDeadline(t time.Time) error {
	<-c.deadline
	err := c.Conn.SetDeadline(t)
	c.deadline <- t
	return err
}

func (c *ConnChecker) SetReadDeadline(t time.Time) error {
	<-c.deadline
	err := c.Conn.SetReadDeadline(t)
	c.deadline <- t
	return err
}
