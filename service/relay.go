package service

import (
	"io"
	"net"
	"os"
	"time"
)

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
