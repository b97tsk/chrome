package chrome

import (
	"io"
	"net"
	"sync"
	"time"
)

func Relay(c1, c2 net.Conn) {
	var wg sync.WaitGroup

	wg.Add(2)

	copy := func(c1, c2 net.Conn) {
		defer wg.Done()

		b := relayPool.Get().(*relayBuffer)
		defer relayPool.Put(b)

		_, _ = io.CopyBuffer(c1, c2, (*b)[:])

		_ = c1.SetReadDeadline(time.Now())
	}

	go copy(c1, c2)
	go copy(c2, c1)

	wg.Wait()
}

type relayBuffer [32 * 1024]byte

var relayPool = sync.Pool{
	New: func() interface{} { return new(relayBuffer) },
}
