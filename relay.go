package chrome

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/b97tsk/chrome/internal/netutil"
)

// RelayOptions provides options for Relay.
type RelayOptions struct {
	// Timeout for each attempt to relay.
	//
	// After the remote-side connection has been established, we send a request
	// to the remote and normally we can expect the remote sends back a response.
	//
	// However, if the connection was established via a proxy, we cannot be sure
	// that we have successfully connected to the remote. A proxy can certainly
	// delay the actual work and return a connection early for a good reason.
	//
	// Relay detects that if the remote does not send back a response within
	// a period of time, it kills the connection and resends the request to
	// a new one.
	Timeout time.Duration
	// Interval specifies the minimum interval between two consecutive attempts.
	// If one attempt fails shortly, next attempt has to wait.
	Interval time.Duration
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
		Timeout      atomic.Int64
		Interval     atomic.Int64
		ConnIdle     atomic.Int64
		UplinkIdle   atomic.Int64
		DownlinkIdle atomic.Int64
	}
}

// SetRelayOptions sets default options for Relay, which may be overrided when
// Relay.
func (m *relayService) SetRelayOptions(opts RelayOptions) {
	m.relayOpts.Timeout.Store(int64(opts.Timeout))
	m.relayOpts.Interval.Store(int64(opts.Interval))
	m.relayOpts.ConnIdle.Store(int64(opts.ConnIdle))
	m.relayOpts.UplinkIdle.Store(int64(opts.UplinkIdle))
	m.relayOpts.DownlinkIdle.Store(int64(opts.DownlinkIdle))
}

// Relay relays two (TCP) connections, that is, read from one and write to
// the other, in both directions. In addition, Relay accepts a RelayOptions
// that can be specified with opts parameter or by SetRelayOptions method.
func (m *relayService) Relay(
	local net.Conn,
	getRemote func(context.Context) net.Conn,
	sendResponse func(io.Writer) bool,
	opts RelayOptions,
) {
	local, localCtx := netutil.NewConnChecker(local)
	defer local.Close()

	r := netutil.NewConnReplayer(local)
	local = r

	if opts.Timeout <= 0 {
		opts.Timeout = time.Duration(m.relayOpts.Timeout.Load())
		if opts.Timeout <= 0 {
			opts.Timeout = defaultRelayTimeout
		}
	}

	if opts.Interval <= 0 {
		opts.Interval = time.Duration(m.relayOpts.Interval.Load())
		if opts.Interval <= 0 {
			opts.Interval = defaultRelayInterval
		}
	}

	try := func() (again bool) {
		remote := getRemote(localCtx)
		if remote == nil {
			return
		}
		defer remote.Close()

		if sendResponse != nil {
			if !sendResponse(local) {
				return
			}

			sendResponse = nil
		}

		defer time.AfterFunc(opts.Timeout, func() {
			if !r.Stopped() {
				aLongTimeAgo := time.Unix(1, 0)
				_ = remote.SetReadDeadline(aLongTimeAgo)
			}
		}).Stop()

		do := func(int) {
			if !r.Stopped() {
				r.Stop()
			}
		}

		startTime := time.Now()

		m.relay(local, netutil.DoR(remote, do), opts)

		if !r.Replay() {
			return
		}

		if d := time.Since(startTime); d < opts.Interval {
			select {
			case <-time.After(opts.Interval - d):
			case <-localCtx.Done():
			}
		}

		return localCtx.Err() == nil
	}

	for try() {
		continue
	}
}

func (m *relayService) relay(l, r net.Conn, opts RelayOptions) {
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
			_ = dst.SetReadDeadline(aLongTimeAgo)

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

	t := time.NewTimer(td)
	defer t.Stop()

	for {
		select {
		case <-t.C:
			expired = true
			aLongTimeAgo := time.Unix(1, 0)
			_ = l.SetReadDeadline(aLongTimeAgo)
			_ = r.SetReadDeadline(aLongTimeAgo)
		case d, ok := <-reset:
			if !ok {
				var noDeadline time.Time
				_ = l.SetReadDeadline(noDeadline)
				_ = r.SetReadDeadline(noDeadline)

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
	New: func() any { return new(relayBuffer) },
}

const (
	defaultRelayTimeout      = 5 * time.Minute
	defaultRelayInterval     = 2500 * time.Millisecond
	defaultRelayConnIdle     = 5 * time.Minute
	defaultRelayUplinkIdle   = 2 * time.Second
	defaultRelayDownlinkIdle = 5 * time.Second
)
