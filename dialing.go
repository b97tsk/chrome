package chrome

import (
	"context"
	"math/bits"
	"net"
	"sync/atomic"
	"time"

	"github.com/b97tsk/log"
	"github.com/b97tsk/proxy"
)

// DialOptions provides options for Dial.
type DialOptions struct {
	// Timeout for each attempt to dial.
	Timeout time.Duration
	// Interval specifies the minimum interval between two consecutive attempts.
	// If one attempt fails shortly, next attempt has to wait.
	Interval time.Duration
	// MaxAttempts specifies the maximum number of dials.
	MaxAttempts int
}

type dialingService struct {
	dialOpts struct {
		Timeout     atomic.Int64
		Interval    atomic.Int64
		MaxAttempts atomic.Int64
	}
}

// SetDialOptions sets default options for Dial, which may be overrided when
// Dial.
func (m *dialingService) SetDialOptions(opts DialOptions) {
	m.dialOpts.Timeout.Store(int64(opts.Timeout))
	m.dialOpts.Interval.Store(int64(opts.Interval))
	m.dialOpts.MaxAttempts.Store(int64(opts.MaxAttempts))
}

// Dial dials address with dialer repeatedly until success or ctx is canceled.
// Dial accepts a DialOptions that can be specified with opts parameter or
// by SetDialOptions method.
func (m *dialingService) Dial(
	ctx context.Context,
	dialer proxy.Dialer,
	network, address string,
	opts DialOptions,
	logger *log.Logger,
) (c net.Conn, err error) {
	if dialer == nil {
		dialer = proxy.Direct
	}

	if opts.Timeout <= 0 {
		opts.Timeout = time.Duration(m.dialOpts.Timeout.Load())
		if opts.Timeout <= 0 {
			opts.Timeout = defaultDialTimeout
		}
	}

	if opts.Interval <= 0 {
		opts.Interval = time.Duration(m.dialOpts.Interval.Load())
		if opts.Interval <= 0 {
			opts.Interval = defaultDialInterval
		}
	}

	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = int(m.dialOpts.MaxAttempts.Load())
		if opts.MaxAttempts <= 0 {
			opts.MaxAttempts = defaultDialMaxAttempts
		}
	}

	attempts := 0
	es, esc := "", 0

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		startTime := time.Now()
		temp, cancel := context.WithDeadline(ctx, startTime.Add(opts.Timeout))
		c, err = proxy.Dial(temp, dialer, network, address)

		attempts++

		cancel()

		if err == nil {
			return
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if logger != nil && logger.TraceWritable() {
			if s := err.Error(); s != es {
				es, esc = s, 1
			} else {
				esc++
			}

			switch {
			case esc == 1:
				logger.Tracef("dial(%v) %v: %v", attempts, address, es)
			case bits.OnesCount(uint(esc)) == 1 || esc&15 == 0:
				logger.Tracef("dial(%v) %v: %v (x%v)", attempts, address, es, esc)
			}
		}

		if d := time.Since(startTime); d < opts.Interval {
			select {
			case <-time.After(opts.Interval - d):
			case <-ctx.Done():
			}
		}

		if attempts == opts.MaxAttempts {
			<-ctx.Done()
		}
	}
}

const (
	defaultDialTimeout     = 10 * time.Second
	defaultDialInterval    = 2500 * time.Millisecond
	defaultDialMaxAttempts = 99
)
