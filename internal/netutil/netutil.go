package netutil

import "net"

func DoR(c net.Conn, do func(int)) net.Conn {
	return &doOnReadConn{c, do}
}

type doOnReadConn struct {
	net.Conn
	do func(int)
}

func (c *doOnReadConn) Read(p []byte) (n int, err error) {
	n, err = c.Conn.Read(p)
	if n > 0 {
		c.do(n)
	}

	return
}
