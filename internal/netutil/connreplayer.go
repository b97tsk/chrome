package netutil

import (
	"errors"
	"net"
	"os"
	"sync/atomic"
)

type ConnReplayer struct {
	net.Conn

	data    []byte
	avail   []byte
	stopped atomic.Bool
}

func NewConnReplayer(c net.Conn) *ConnReplayer {
	return &ConnReplayer{Conn: c}
}

func (c *ConnReplayer) Stop() {
	c.stopped.Store(true)
}

func (c *ConnReplayer) Stopped() bool {
	return c.stopped.Load()
}

func (c *ConnReplayer) Replay() bool {
	if len(c.data) != 0 && !c.Stopped() {
		c.avail = c.data
		return true
	}

	return false
}

func (c *ConnReplayer) Read(b []byte) (n int, err error) {
	if avail := c.avail; len(avail) != 0 {
		n = copy(b, avail)

		avail = avail[n:]
		if len(avail) == 0 {
			avail = nil
		}

		c.avail = avail

		return
	}

	n, err = c.Conn.Read(b)

	if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		c.Stop()
	}

	if c.Stopped() {
		c.data = nil
		return
	}

	c.data = append(c.data, b[:n]...)

	return
}
