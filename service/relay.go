package service

import (
	"io"
	"net"
	"os"
	"sync"
	"time"
)

func Relay(left, right net.Conn) error {
	c := make(chan error, 2)
	go func() {
		buf := relayPool.Get()
		defer relayPool.Put(buf)
		_, err := io.CopyBuffer(left, right, (*buf.(*relayBuffer))[:])
		left.SetReadDeadline(time.Now())
		c <- err
	}()
	go func() {
		buf := relayPool.Get()
		defer relayPool.Put(buf)
		_, err := io.CopyBuffer(right, left, (*buf.(*relayBuffer))[:])
		right.SetReadDeadline(time.Now())
		c <- err
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

type relayBuffer [32 * 1024]byte

var relayPool = sync.Pool{
	New: func() interface{} { return &relayBuffer{} },
}
