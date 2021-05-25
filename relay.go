package chrome

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/b97tsk/chrome/internal/netutil"
)

func Relay(l, r net.Conn) {
	const ConnIdle = 300 * time.Second
	const UplinkIdle = 2 * time.Second
	const DownlinkIdle = 5 * time.Second

	reset := make(chan time.Duration, 1)

	num := int32(2)
	done := func() {
		if atomic.AddInt32(&num, -1) == 0 {
			close(reset)
		}
	}

	copy := func(dst, src net.Conn, idle time.Duration) {
		defer done()

		b := relayPool.Get().(*relayBuffer)
		defer relayPool.Put(b)

		if _, err := io.CopyBuffer(dst, src, (*b)[:]); err != nil {
			_ = dst.SetReadDeadline(time.Now())
			return
		}

		reset <- idle
	}

	do := func(int) {
		select {
		case reset <- -1:
		default:
		}
	}

	go copy(l, netutil.DoR(r, do), UplinkIdle)
	go copy(r, netutil.DoR(l, do), DownlinkIdle)

	expired := false
	td := ConnIdle

	t := time.AfterFunc(td, func() {
		now := time.Now()
		_ = l.SetReadDeadline(now)
		_ = r.SetReadDeadline(now)
	})
	defer t.Stop()

	for {
		select {
		case <-t.C:
			expired = true
		case d := <-reset:
			if d == 0 {
				return
			}

			if d > 0 {
				td = d
			}

			if !expired {
				t.Reset(td)
			}
		}
	}
}

type relayBuffer [32 * 1024]byte

var relayPool = sync.Pool{
	New: func() interface{} { return new(relayBuffer) },
}
