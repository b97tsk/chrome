package chrome

import (
	"context"
	"net"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/b97tsk/chrome/internal/proxy"
)

func (man *Manager) Dial(
	ctx context.Context,
	dialer proxy.Dialer,
	network, address string,
	timeout time.Duration,
) (conn net.Conn, err error) {
	return man.builtin.Dial(ctx, dialer, network, address, timeout)
}

type dialingService struct {
	dialTimeout [3]uint32
}

func (d *dialingService) DialTimeout() time.Duration {
	ptr := (*int64)(unsafe.Pointer(uintptr(unsafe.Pointer(&d.dialTimeout[1])) &^ 4))
	return time.Duration(atomic.LoadInt64(ptr))
}

func (d *dialingService) SetDialTimeout(timeout time.Duration) {
	ptr := (*int64)(unsafe.Pointer(uintptr(unsafe.Pointer(&d.dialTimeout[1])) &^ 4))
	atomic.StoreInt64(ptr, int64(timeout))
}

func (d *dialingService) Dial(
	ctx context.Context,
	dialer proxy.Dialer,
	network, address string,
	timeout time.Duration,
) (conn net.Conn, err error) {
	if dialer == nil {
		dialer = proxy.Direct
	}

	if timeout <= 0 {
		timeout = d.DialTimeout()
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

		if err == nil || !isTemporary(err) {
			return
		}
	}
}

const defaultDialTimeout = 30 * time.Second
