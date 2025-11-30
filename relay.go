package chrome

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// RelayOptions provides options for Relay.
type RelayOptions struct {
	// ConnIdle is the idle timeout when Relay starts.
	// If both connections (local-side and remote-side) remains idle (no reads)
	// for the duration of ConnIdle, both are closed and Relay ends.
	ConnIdle time.Duration
	// UplinkIdle is the idle timeout when the remote-side connection (downlink)
	// closes. If the local-side connection (uplink) remains idle (no reads)
	// for the duration of UplinkIdle, it is closed and Relay ends.
	UplinkIdle time.Duration
	// DownlinkIdle is the idle timeout when the local-side connection (uplink)
	// closes. If the remote-side connection (downlink) remains idle (no reads)
	// for the duration of DownlinkIdle, it is closed and Relay ends.
	DownlinkIdle time.Duration
}

type relayService struct {
	relayOpts struct {
		ConnIdle     atomic.Int64
		UplinkIdle   atomic.Int64
		DownlinkIdle atomic.Int64
	}
}

// SetRelayOptions sets default options for Relay, which may be overrided when
// Relay.
func (m *relayService) SetRelayOptions(opts RelayOptions) {
	m.relayOpts.ConnIdle.Store(int64(opts.ConnIdle))
	m.relayOpts.UplinkIdle.Store(int64(opts.UplinkIdle))
	m.relayOpts.DownlinkIdle.Store(int64(opts.DownlinkIdle))
}

// Relay (TCP only) sends packets from local to remote, and vice versa.
func (m *relayService) Relay(l, r net.Conn, opts RelayOptions) {
	if opts.ConnIdle <= 0 {
		opts.ConnIdle = time.Duration(m.relayOpts.ConnIdle.Load())
		if opts.ConnIdle <= 0 {
			opts.ConnIdle = defaultRelayConnIdle
		}
	}
	if opts.UplinkIdle <= 0 {
		opts.UplinkIdle = time.Duration(m.relayOpts.UplinkIdle.Load())
		if opts.UplinkIdle <= 0 {
			opts.UplinkIdle = defaultRelayUplinkIdle
		}
	}
	if opts.DownlinkIdle <= 0 {
		opts.DownlinkIdle = time.Duration(m.relayOpts.DownlinkIdle.Load())
		if opts.DownlinkIdle <= 0 {
			opts.DownlinkIdle = defaultRelayDownlinkIdle
		}
	}

	reset := make(chan time.Duration, 1)

	var n atomic.Uint32

	done := func() {
		if n.Add(1) == 2 {
			close(reset)
		}
	}

	copy := func(dst, src net.Conn, idle time.Duration) {
		defer done()

		b := relayPool.Get().(*relayBuffer)
		defer relayPool.Put(b)

		if _, err := io.CopyBuffer(dst, src, (*b)[:]); err != nil {
			aLongTimeAgo := time.Unix(1, 0)
			dst.SetReadDeadline(aLongTimeAgo)
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

	go copy(l, doOnRead(r, do), opts.UplinkIdle)
	go copy(r, doOnRead(l, do), opts.DownlinkIdle)

	expired := false
	td := opts.ConnIdle

	t := time.NewTimer(td)
	defer t.Stop()

	for {
		select {
		case <-t.C:
			expired = true
			aLongTimeAgo := time.Unix(1, 0)
			l.SetReadDeadline(aLongTimeAgo)
			r.SetReadDeadline(aLongTimeAgo)
		case d, ok := <-reset:
			if !ok {
				var noDeadline time.Time
				l.SetReadDeadline(noDeadline)
				r.SetReadDeadline(noDeadline)
				return
			}
			if d >= 0 {
				td = d
			}
			if !expired {
				t.Reset(td)
			}
		}
	}
}

func doOnRead(c net.Conn, do func(int)) net.Conn {
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

type relayBuffer [32 * 1024]byte

var relayPool = sync.Pool{
	New: func() any { return new(relayBuffer) },
}

const (
	defaultRelayConnIdle     = 5 * time.Minute
	defaultRelayUplinkIdle   = 0
	defaultRelayDownlinkIdle = 0
)
