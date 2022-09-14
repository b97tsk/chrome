package chrome

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"time"

	"github.com/b97tsk/proxy"
)

type dialingService struct {
	dialTimeout atomic.Int64
}

// DialTimeout gets the dial timeout.
func (m *dialingService) DialTimeout() time.Duration {
	return time.Duration(m.dialTimeout.Load())
}

// SetDialTimeout sets the dial timeout, which may be overrided when Dial.
func (m *dialingService) SetDialTimeout(timeout time.Duration) {
	m.dialTimeout.Store(int64(timeout))
}

// actualDialTimeout returns the dial timeout in effect.
func (m *dialingService) actualDialTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		timeout = m.DialTimeout()
		if timeout <= 0 {
			timeout = defaultDialTimeout
		}
	}

	return timeout
}

// Dial dials specified address with dialer repeatedly until success or
// ctx is canceled or dialer returns a non-timeout error.
// Each dial has a timeout that can be specified with timeout parameter or
// by SetDialTimeout method.
func (m *dialingService) Dial(
	ctx context.Context,
	dialer proxy.Dialer,
	network, address string,
	timeout time.Duration,
) (conn net.Conn, err error) {
	if dialer == nil {
		dialer = proxy.Direct
	}

	for {
		err = ctx.Err()
		if err != nil {
			return
		}

		ctx, cancel := context.WithTimeout(ctx, m.actualDialTimeout(timeout))

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

const defaultDialTimeout = 10 * time.Second
