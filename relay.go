package chrome

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/b97tsk/chrome/internal/netutil"
)

// RelayOptions provides options for relay.
type RelayOptions struct {
	// ConnIdle is the idle timeout when relay starts.
	// If both connections (local-side and remote-side) remains idle (no reads)
	// for the duration of ConnIdle, both are closed and relay ends.
	ConnIdle time.Duration
	// UplinkIdle is the idle timeout when the remote-side connection (downlink)
	// closes. If the local-side connection (uplink) remains idle (no reads)
	// for the duration of UplinkIdle, it is closed and relay ends.
	UplinkIdle time.Duration
	// DownlinkIdle is the idle timeout when the local-side connection (uplink)
	// closes. If the remote-side connection (downlink) remains idle (no reads)
	// for the duration of DownlinkIdle, it is closed and relay ends.
	DownlinkIdle time.Duration
}

type relayService struct {
	opts [7]uint32
}

func (m *relayService) connIdle() time.Duration {
	ptr := (*int64)(unsafe.Pointer(uintptr(unsafe.Pointer(&m.opts[1])) &^ 4))
	return time.Duration(atomic.LoadInt64(ptr))
}

func (m *relayService) setConnIdle(idle time.Duration) {
	ptr := (*int64)(unsafe.Pointer(uintptr(unsafe.Pointer(&m.opts[1])) &^ 4))
	atomic.StoreInt64(ptr, int64(idle))
}

func (m *relayService) uplinkIdle() time.Duration {
	ptr := (*int64)(unsafe.Pointer(uintptr(unsafe.Pointer(&m.opts[3])) &^ 4))
	return time.Duration(atomic.LoadInt64(ptr))
}

func (m *relayService) setUplinkIdle(idle time.Duration) {
	ptr := (*int64)(unsafe.Pointer(uintptr(unsafe.Pointer(&m.opts[3])) &^ 4))
	atomic.StoreInt64(ptr, int64(idle))
}

func (m *relayService) downlinkIdle() time.Duration {
	ptr := (*int64)(unsafe.Pointer(uintptr(unsafe.Pointer(&m.opts[5])) &^ 4))
	return time.Duration(atomic.LoadInt64(ptr))
}

func (m *relayService) setDownlinkIdle(idle time.Duration) {
	ptr := (*int64)(unsafe.Pointer(uintptr(unsafe.Pointer(&m.opts[5])) &^ 4))
	atomic.StoreInt64(ptr, int64(idle))
}

// SetRelayOptions sets the relay options, which may be overrided when Relay.
func (m *relayService) SetRelayOptions(opts RelayOptions) {
	m.setConnIdle(opts.ConnIdle)
	m.setUplinkIdle(opts.UplinkIdle)
	m.setDownlinkIdle(opts.DownlinkIdle)
}

// Relay relays two (TCP) connections, that is, read from one and write to
// the other, in both directions. In addition, Relay accepts a RelayOptions
// that can be specified with opts parameter or by SetRelayOptions method.
func (m *relayService) Relay(l, r net.Conn, opts RelayOptions) {
	if opts.ConnIdle <= 0 {
		opts.ConnIdle = m.connIdle()
		if opts.ConnIdle <= 0 {
			opts.ConnIdle = defaultConnIdle
		}
	}

	if opts.UplinkIdle <= 0 {
		opts.UplinkIdle = m.uplinkIdle()
		if opts.UplinkIdle <= 0 {
			opts.UplinkIdle = defaultUplinkIdle
		}
	}

	if opts.DownlinkIdle <= 0 {
		opts.DownlinkIdle = m.downlinkIdle()
		if opts.DownlinkIdle <= 0 {
			opts.DownlinkIdle = defaultDownlinkIdle
		}
	}

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

	go copy(l, netutil.DoR(r, do), opts.UplinkIdle)
	go copy(r, netutil.DoR(l, do), opts.DownlinkIdle)

	expired := false
	td := opts.ConnIdle

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

const (
	defaultConnIdle     = 300 * time.Second
	defaultUplinkIdle   = 2 * time.Second
	defaultDownlinkIdle = 5 * time.Second
)
