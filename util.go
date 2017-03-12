package main

import (
	"encoding/base64"
	"io"
	"net"
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

// relay copies between left and right bidirectionally. Returns number of
// bytes copied from right to left, from left to right, and any error occurred.
func relay(left, right net.Conn) (int64, int64, error) {
	type result struct {
		N   int64
		Err error
	}
	ch := make(chan result)

	go func() {
		n, err := io.Copy(right, left)
		right.SetReadDeadline(time.Now())
		ch <- result{n, err}
	}()

	n, err := io.Copy(left, right)
	left.SetReadDeadline(time.Now())
	res := <-ch

	if err == nil {
		err = res.Err
	}
	return n, res.N, err
}
