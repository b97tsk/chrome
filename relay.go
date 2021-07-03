package chrome

import (
	"context"
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
	// After the remote-side connection has been established, we send a request
	// to the remote and normally we can expect the remote sends back a response.
	// But if the connection was established via a proxy, we cannot be sure that
	// we have successfully connected to the remote. A proxy certainly can delay
	// the actual work for a good reason.
	//
	// Replay mode detects that if the remote does not send back a response
	// within a period of time, we kill the connection and resend the request
	// to a new one.
	//
	// Replay mode is on by default. It should work fine even not using proxies.
	Replay struct {
		Enabled  bool
		Disabled bool
		Response struct {
			// Timeout fallbacks to global dial timeout if not set.
			Timeout time.Duration
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
		Disabled uint32
		_        uint32
		Response struct {
			Timeout time.Duration
		}
	}
	connIdle     time.Duration
	uplinkIdle   time.Duration
	downlinkIdle time.Duration
}

func loadDuration(addr *time.Duration) time.Duration {
	ptr := (*int64)(unsafe.Pointer(uintptr(unsafe.Pointer(addr)) &^ 4))
	return time.Duration(atomic.LoadInt64(ptr))
}

func storeDuration(addr *time.Duration, d time.Duration) {
	ptr := (*int64)(unsafe.Pointer(uintptr(unsafe.Pointer(addr)) &^ 4))
	atomic.StoreInt64(ptr, int64(d))
}

func (m *relayService) getReplayDisabled() bool {
	return atomic.LoadUint32(&m.replay.Disabled) != 0
}

func (m *relayService) setReplayDisabled(disabled bool) {
	var replayDisabled uint32
	if disabled {
		replayDisabled = 1
	}

	atomic.StoreUint32(&m.replay.Disabled, replayDisabled)
}

// SetRelayOptions sets the relay options, which may be overrided when Relay.
func (m *relayService) SetRelayOptions(opts RelayOptions) {
	m.setReplayDisabled(opts.Replay.Disabled)
	storeDuration(&m.replay.Response.Timeout, opts.Replay.Response.Timeout)
	storeDuration(&m.connIdle, opts.ConnIdle)
	storeDuration(&m.uplinkIdle, opts.UplinkIdle)
	storeDuration(&m.downlinkIdle, opts.DownlinkIdle)
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
	if !opts.Replay.Enabled && (opts.Replay.Disabled || m.getReplayDisabled()) {
		m.relayWithoutReplay(local, getRemote, sendResponse, opts)
		return
	}

	m.relayWithReplay(local, getRemote, sendResponse, opts)
}

func (m *Manager) relayWithoutReplay(
	local net.Conn,
	getRemote func(context.Context) net.Conn,
	sendResponse func(io.Writer) bool,
	opts RelayOptions,
) {
	local, localCtx := NewConnChecker(local)
	defer local.Close()

	remote := getRemote(localCtx)
	if remote == nil {
		return
	}
	defer remote.Close()

	if sendResponse != nil && !sendResponse(local) {
		return
	}

	m.relay(local, remote, opts)
}

func (m *Manager) relayWithReplay(
	local net.Conn,
	getRemote func(context.Context) net.Conn,
	sendResponse func(io.Writer) bool,
	opts RelayOptions,
) {
	local, localCtx := NewConnChecker(local)
	defer local.Close()

	replayer := newConnReplayer(local)
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
			timeout = m.actualDialTimeout(loadDuration(&m.replay.Response.Timeout))
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

		m.relay(local, netutil.DoR(remote, do), opts)

		return replayer.Replay() && localCtx.Err() == nil
	}

	for try() {
		continue
	}
}

func (m *relayService) relay(l, r net.Conn, opts RelayOptions) {
	if opts.ConnIdle <= 0 {
		opts.ConnIdle = loadDuration(&m.connIdle)
		if opts.ConnIdle <= 0 {
			opts.ConnIdle = defaultConnIdle
		}
	}

	if opts.UplinkIdle <= 0 {
		opts.UplinkIdle = loadDuration(&m.uplinkIdle)
		if opts.UplinkIdle <= 0 {
			opts.UplinkIdle = defaultUplinkIdle
		}
	}

	if opts.DownlinkIdle <= 0 {
		opts.DownlinkIdle = loadDuration(&m.downlinkIdle)
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
	defaultConnIdle     = 300 * time.Second
	defaultUplinkIdle   = 2 * time.Second
	defaultDownlinkIdle = 5 * time.Second
)
