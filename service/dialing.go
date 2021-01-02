package service

import (
	"context"
	"net"
	"sync/atomic"
	"time"

	"github.com/b97tsk/chrome/internal/proxy"
)

func (man *Manager) Dial(
	ctx context.Context,
	dialer proxy.Dialer,
	network, address string,
	timeout time.Duration,
) (conn net.Conn, err error) {
	return man.dial(ctx, dialer, network, address, timeout)
}

type dialingService struct {
	dialTimeout int64
}

func (d *dialingService) setDialTimeout(timeout time.Duration) {
	atomic.StoreInt64(&d.dialTimeout, int64(timeout))
}

func (d *dialingService) dial(
	ctx context.Context,
	dialer proxy.Dialer,
	network, address string,
	timeout time.Duration,
) (conn net.Conn, err error) {
	if dialer == nil {
		dialer = proxy.Direct
	}

	dialTimeout := defaultDialTimeout
	if timeout > 0 {
		dialTimeout = timeout
	} else if timeout := atomic.LoadInt64(&d.dialTimeout); timeout > 0 {
		dialTimeout = time.Duration(timeout)
	}

	for {
		err = ctx.Err()
		if err != nil {
			return
		}

		ctx, cancel := context.WithTimeout(ctx, dialTimeout)

		conn, err = proxy.Dial(ctx, dialer, network, address)

		cancel()

		if err == nil || !isTemporary(err) {
			return
		}
	}
}

const defaultDialTimeout = 30 * time.Second
