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

// RelayOptions provides options for relay.
type RelayOptions struct {
	// After the remote-side connection has been established, we send a request
	// to the remote and normally we can expect the remote sends back a response.
	//
	// However, if the connection was established via a proxy, we cannot be sure
	// that we have successfully connected to the remote. A proxy can certainly
	// delay the actual work and return a connection early for a good reason.
	//
	// Replay mode detects that if the remote does not send back a response
	// within a period of time, we kill the connection and resend the request
	// to a new one.
	//
	// Replay mode is always on. If you want it off, you can just set response
	// timeout to a large value (5 minutes, which is the default, should be enough).
	Replay struct {
		Response struct {
			Timeout time.Duration
		}
		Retry struct {
			Always *bool
		}
	}
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
	replay struct {
		Response struct {
			Timeout atomic.Int64
		}
		Retry struct {
			Always atomic.Uint32
		}
	}
	connIdle     atomic.Int64
	uplinkIdle   atomic.Int64
	downlinkIdle atomic.Int64
}

func boolPtrToUint32(v *bool, def uint32) uint32 {
	switch {
	case v == nil:
		return def
	case *v:
		return 1
	default:
		return 0
	}
}

// SetRelayOptions sets the relay options, which may be overrided when Relay.
func (m *relayService) SetRelayOptions(opts RelayOptions) {
	m.replay.Response.Timeout.Store(int64(opts.Replay.Response.Timeout))
	m.replay.Retry.Always.Store(boolPtrToUint32(opts.Replay.Retry.Always, 0))
	m.connIdle.Store(int64(opts.ConnIdle))
	m.uplinkIdle.Store(int64(opts.UplinkIdle))
	m.downlinkIdle.Store(int64(opts.DownlinkIdle))
}

// Relay relays two (TCP) connections, that is, read from one and write to
// the other, in both directions. In addition, Relay accepts a RelayOptions
// that can be specified with opts parameter or by SetRelayOptions method.
func (m *Manager) Relay(
	local net.Conn,
	getRemote func(context.Context) net.Conn,
	sendResponse func(io.Writer) bool,
	opts RelayOptions,
) {
	local, localCtx := netutil.NewConnChecker(local)
	defer local.Close()

	replayer := netutil.NewConnReplayer(local)
	local = replayer

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

		timeout := opts.Replay.Response.Timeout
		if timeout <= 0 {
			timeout = time.Duration(m.replay.Response.Timeout.Load())
			if timeout <= 0 {
				timeout = defaultResponseTimeout
			}
		}

		defer time.AfterFunc(timeout, func() {
			if !replayer.Stopped() {
				aLongTimeAgo := time.Unix(1, 0)
				_ = remote.SetReadDeadline(aLongTimeAgo)
			}
		}).Stop()

		do := func(int) {
			if !replayer.Stopped() {
				replayer.Stop()
			}
		}

		startTime := time.Now()

		m.relay(local, netutil.DoR(remote, do), opts)

		if time.Since(startTime) < timeout {
			retryAlways := boolPtrToUint32(
				opts.Replay.Retry.Always,
				m.replay.Retry.Always.Load(),
			)
			if retryAlways == 0 {
				return false
			}
		}

		return replayer.Replay() && localCtx.Err() == nil
	}

	for try() {
		continue
	}
}

func (m *relayService) relay(l, r net.Conn, opts RelayOptions) {
	if opts.ConnIdle <= 0 {
		opts.ConnIdle = time.Duration(m.connIdle.Load())
		if opts.ConnIdle <= 0 {
			opts.ConnIdle = defaultConnIdle
		}
	}

	if opts.UplinkIdle <= 0 {
		opts.UplinkIdle = time.Duration(m.uplinkIdle.Load())
		if opts.UplinkIdle <= 0 {
			opts.UplinkIdle = defaultUplinkIdle
		}
	}

	if opts.DownlinkIdle <= 0 {
		opts.DownlinkIdle = time.Duration(m.downlinkIdle.Load())
		if opts.DownlinkIdle <= 0 {
			opts.DownlinkIdle = defaultDownlinkIdle
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
	New: func() interface{} { return new(relayBuffer) },
}

const (
	defaultResponseTimeout = 5 * time.Minute

	defaultConnIdle     = 5 * time.Minute
	defaultUplinkIdle   = 2 * time.Second
	defaultDownlinkIdle = 5 * time.Second
)
