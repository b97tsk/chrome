package service

import (
	"io"
	"net"
	"sync"
	"time"
)

func Relay(c1, c2 net.Conn) {
	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		b := relayPool.Get().(*relayBuffer)
		defer relayPool.Put(b)

		_, _ = io.CopyBuffer(c1, c2, (*b)[:])
		_ = c1.SetReadDeadline(time.Now())
	}()
	go func() {
		defer wg.Done()

		b := relayPool.Get().(*relayBuffer)
		defer relayPool.Put(b)

		_, _ = io.CopyBuffer(c2, c1, (*b)[:])
		_ = c2.SetReadDeadline(time.Now())
	}()
	wg.Wait()
}

type relayBuffer [32 * 1024]byte

var relayPool = sync.Pool{
	New: func() interface{} { return new(relayBuffer) },
}
