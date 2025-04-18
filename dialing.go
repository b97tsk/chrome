package chrome

import (
	"context"
	"errors"
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

// Dial connects address repeatedly until success or ctx is canceled.
//
// For each attempt, Dial calls getopts to obtain a Proxy and a DialOptions.
// Dial uses the Proxy to connect target address, and the DialOptions for
// custom behavior.
func (m *dialingService) Dial(
	ctx context.Context,
	network, address string,
	getopts func() (Proxy, DialOptions, bool),
	logger *log.Logger,
) (c net.Conn, err error) {
	getaddr := func() string { return address }
	return m.Dialv2(ctx, network, getaddr, getopts, logger)
}

// Dialv2 connects an address returned by getaddr() repeatedly until success
// or ctx is canceled.
//
// For each attempt, Dialv2 calls getaddr to obtain an address, and getopts
// to obtain a Proxy and a DialOptions.
// Dialv2 uses the Proxy to connect the address, and the DialOptions for
// custom behavior.
func (m *dialingService) Dialv2(
	ctx context.Context,
	network string,
	getaddr func() string,
	getopts func() (Proxy, DialOptions, bool),
	logger *log.Logger,
) (c net.Conn, err error) {
	attempts := 0
	es, esc := "", 0

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		p, opts, ok := getopts()
		if !ok {
			return nil, errDismissed
		}

		dialer := p.Dialer()

		if block, ok := dialer.(blockOrReset); ok {
			if block {
				<-ctx.Done()
				return nil, ctx.Err()
			}

			return nil, errReset
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

		if attempts >= opts.MaxAttempts {
			<-ctx.Done()
			return nil, ctx.Err()
		}

		startTime := time.Now()
		dialCtx, cancel := context.WithCancel(ctx)
		timer := time.AfterFunc(opts.Timeout, cancel)
		address := getaddr()
		c, err = proxy.Dial(dialCtx, dialer, network, address)
		timerStopped := timer.Stop()

		attempts++

		if err == nil && timerStopped {
			// We probably should not cancel dialCtx here, as some buggy dialer might
			// return connections that still rely on it.
			// timerStopped need to be true here, so that dialCtx will not be canceled
			// by timer.
			return
		}

		if c != nil {
			_ = c.Close()
		}

		if timerStopped {
			cancel()
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if err != nil && logger != nil && logger.TraceWritable() {
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
	}
}

const (
	defaultDialTimeout     = 10 * time.Second
	defaultDialInterval    = 2500 * time.Millisecond
	defaultDialMaxAttempts = 99
)

var (
	errDismissed = errors.New("dismissed")
	errReset     = errors.New("reset")
)
