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

func Unread(c net.Conn, data []byte) net.Conn {
	return &unreadConn{c, data}
}

type unreadConn struct {
	net.Conn
	data []byte
}

func (c *unreadConn) Read(p []byte) (n int, err error) {
	if data := c.data; len(data) != 0 {
		n = copy(p, data)

		data = data[n:]
		if len(data) == 0 {
			data = nil
		}

		c.data = data

		return
	}

	return c.Conn.Read(p)
}
