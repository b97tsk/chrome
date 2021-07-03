package chrome

import (
	"errors"
	"net"
	"os"
	"sync/atomic"
)

type connReplayer struct {
	net.Conn

	data    []byte
	avail   []byte
	stopped uint32
}

func newConnReplayer(c net.Conn) *connReplayer {
	return &connReplayer{Conn: c}
}

func (c *connReplayer) Stop() {
	atomic.StoreUint32(&c.stopped, 1)
}

func (c *connReplayer) Stopped() bool {
	return atomic.LoadUint32(&c.stopped) != 0
}

func (c *connReplayer) Replay() bool {
	if len(c.data) != 0 && !c.Stopped() {
		c.avail = c.data
		return true
	}

	return false
}

func (c *connReplayer) Read(b []byte) (n int, err error) {
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
