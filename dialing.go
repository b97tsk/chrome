package chrome

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/b97tsk/chrome/internal/proxy"
)

type dialingService struct {
	dialTimeout [3]uint32
}

func (m *dialingService) DialTimeout() time.Duration {
	ptr := (*int64)(unsafe.Pointer(uintptr(unsafe.Pointer(&m.dialTimeout[1])) &^ 4))
	return time.Duration(atomic.LoadInt64(ptr))
}

func (m *dialingService) SetDialTimeout(timeout time.Duration) {
	ptr := (*int64)(unsafe.Pointer(uintptr(unsafe.Pointer(&m.dialTimeout[1])) &^ 4))
	atomic.StoreInt64(ptr, int64(timeout))
}

func (m *dialingService) Dial(
	ctx context.Context,
	dialer proxy.Dialer,
	network, address string,
	timeout time.Duration,
) (conn net.Conn, err error) {
	if dialer == nil {
		dialer = proxy.Direct
	}

	if timeout <= 0 {
		timeout = m.DialTimeout()
		if timeout <= 0 {
			timeout = defaultDialTimeout
		}
	}

	for {
		err = ctx.Err()
		if err != nil {
			return
		}

		ctx, cancel := context.WithTimeout(ctx, timeout)

		conn, err = proxy.Dial(ctx, dialer, network, address)

		cancel()

		if err == nil || !isTimeout(err) {
			return
		}
	}
}

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

const defaultDialTimeout = 30 * time.Second
