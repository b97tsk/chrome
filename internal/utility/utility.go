package utility

import (
	"encoding/base64"
	"io"
	"net"
	"os"
	"time"
)

func DecodeBase64String(s string) ([]byte, error) {
	enc := base64.StdEncoding
	if len(s)%4 != 0 {
		enc = base64.RawStdEncoding
	}
	return enc.DecodeString(s)
}

func TCPKeepAlive(c net.Conn, d time.Duration) {
	if tcp, ok := c.(*net.TCPConn); ok && d > 0 {
		tcp.SetKeepAlive(true)
		tcp.SetKeepAlivePeriod(d)
	}
}

func IsTemporary(err error) bool {
	e, ok := err.(interface {
		Temporary() bool
	})
	return ok && e.Temporary()
}

func Relay(left, right net.Conn) error {
	c := make(chan error, 2)
	go func() {
		_, err := io.Copy(left, right)
		c <- err
		left.SetReadDeadline(time.Now())
	}()
	go func() {
		_, err := io.Copy(right, left)
		c <- err
		right.SetReadDeadline(time.Now())
	}()
	e1, e2 := <-c, <-c
	if e1 != nil && !os.IsTimeout(e1) {
		return e1
	}
	if e2 != nil && !os.IsTimeout(e2) {
		return e2
	}
	return nil
}
