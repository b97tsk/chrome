package chrome

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/b97tsk/async"
)

// ConnOptions provides options for NewConn.
type ConnOptions struct {
	// Timeout for each attempt to dial.
	Timeout time.Duration
	// Interval specifies the minimum interval between two consecutive attempts.
	// If one attempt fails shortly, next attempt has to wait.
	Interval time.Duration
	// Parallel specifies the number of attempts that can be made at the same time.
	Parallel int
	// MaxAttempts specifies the maximum number of attempts that can be made for each NewConn call.
	MaxAttempts int
}

type connService struct {
	connOpts struct {
		Timeout     atomic.Int64
		Interval    atomic.Int64
		Parallel    atomic.Int64
		MaxAttempts atomic.Int64
	}
}

// SetConnOptions sets default options for NewConn, which may be overrided
// when NewConn is called.
func (m *connService) SetConnOptions(opts ConnOptions) {
	m.connOpts.Timeout.Store(int64(opts.Timeout))
	m.connOpts.Interval.Store(int64(opts.Interval))
	m.connOpts.Parallel.Store(int64(opts.Parallel))
	m.connOpts.MaxAttempts.Store(int64(opts.MaxAttempts))
}

type ConnEvent any

type (
	ConnEventResponse    struct{}
	ConnEventMaxAttempts struct{}
)

// NewConn returns a [net.Conn] that tries to make a connection to remoteAddr
// repeatedly until success or the maximum number of attempts has been made.
func (m *Manager) NewConn(
	remoteAddr string,
	getRemote func(context.Context) (net.Conn, error),
	connOpts ConnOptions,
	relayOpts RelayOptions,
	logger *slog.Logger,
	lifeCycle func(c <-chan ConnEvent),
) *Conn {
	if connOpts.Timeout <= 0 {
		connOpts.Timeout = time.Duration(m.connOpts.Timeout.Load())
		if connOpts.Timeout <= 0 {
			connOpts.Timeout = defaultConnTimeout
		}
	}
	if connOpts.Interval <= 0 {
		connOpts.Interval = time.Duration(m.connOpts.Interval.Load())
		if connOpts.Interval <= 0 {
			connOpts.Interval = defaultConnInterval
		}
	}
	if connOpts.Parallel <= 0 {
		connOpts.Parallel = int(m.connOpts.Parallel.Load())
		if connOpts.Parallel <= 0 {
			connOpts.Parallel = defaultConnParallel
		}
	}
	if connOpts.MaxAttempts <= 0 {
		connOpts.MaxAttempts = int(m.connOpts.MaxAttempts.Load())
		if connOpts.MaxAttempts <= 0 {
			connOpts.MaxAttempts = defaultConnMaxAttempts
		}
	}

	var myRuntime struct {
		sync.WaitGroup
		async.Executor
	}

	myRuntime.Autorun(func() { myRuntime.Go(myRuntime.Run) })

	logger = logger.With(slog.GroupAttrs("conn",
		slog.String("id", fmt.Sprintf("%p", &myRuntime)),
		slog.String("dest", remoteAddr)))

	var myState struct {
		rr struct { // read request
			done async.State[bool]
			stop async.State[bool]
			req  struct {
				async.Signal
				data []byte
			}
		}
		cl struct { // conn loop
			done        async.State[bool]
			stop        async.State[bool]
			newconn     async.State[bool]
			connections struct {
				async.Signal
				n int
			}
			attempts int
		}
		hasWinner  bool
		sent, recv int64
		remoteAddr net.Addr
	}

	var reqBuf *connBuf

	var lifeCycleCh chan ConnEvent

	if lifeCycle != nil {
		lifeCycleCh = make(chan ConnEvent)
	}

	pipeLeft, pipeRight := net.Pipe()

	myConn := &Conn{Conn: pipeRight}

	stopConnLoop := func(co *async.Coroutine) async.Result {
		myState.cl.stop.Set(true)
		return co.End()
	}

	awaitConnLoop := myState.cl.done.Await(untilTrue)

	stopReadingRequest := func(co *async.Coroutine) async.Result {
		aLongTimeAgo := time.Unix(1, 0)
		pipeLeft.SetReadDeadline(aLongTimeAgo)
		myState.rr.stop.Set(true)
		return co.End()
	}

	awaitReadingRequest := myState.rr.done.Await(untilTrue)

	newConn := func(co *async.Coroutine) async.Result {
		myState.cl.connections.n++
		myState.cl.attempts++
		thisIsTheWinner := false
		ctx, cancel := context.WithCancel(context.Background())
		myRuntime.Spawn(async.Select(
			async.AfterContext(ctx, &myRuntime),
			async.Block(
				awaitConnLoop,
				async.Do(func() {
					if !thisIsTheWinner {
						cancel()
					}
				}),
			),
		))
		myRuntime.Spawn(func(co *async.Coroutine) async.Result {
			timer := time.AfterFunc(connOpts.Timeout, cancel)
			var remote async.State[*conn]
			myRuntime.Go(func() {
				c, err := getRemote(ctx)
				myRuntime.Spawn(func(co *async.Coroutine) async.Result {
					var rc *conn
					if c != nil {
						rc = &conn{Conn: c}
					}
					remote.Set(rc)
					if errors.Is(err, CloseConn) {
						return co.Transition(stopReadingRequest)
					}
					return co.End()
				})
				if err != nil && !errors.Is(err, context.Canceled) {
					logger.Debug("conn:attempt", slog.Any("error", err))
				}
			})
			co.Defer(
				async.Do(func() {
					if c := remote.Get(); c != nil {
						c.Close()
					}
					timer.Stop()
					cancel()
					myState.cl.connections.n--
					myState.cl.connections.Notify()
				}),
			)
			return co.Await(&remote).Then(
				func(co *async.Coroutine) async.Result {
					if remote.Get() == nil {
						return co.End()
					}
					var sr struct { // send request
						done async.State[bool]
						stop async.State[bool]
						sent int
					}
					sendRequest := func() {
						var result struct {
							async.Signal
							n   int
							err error
						}
						myRuntime.Spawn(async.Block(
							async.Defer(async.Do(func() { sr.done.Set(true) })),
							async.Loop(async.Block(
								func(co *async.Coroutine) async.Result {
									if sr.stop.Get() {
										return co.Break()
									}
									co.Watch(&sr.stop)
									if sr.sent == len(myState.rr.req.data) {
										return co.Yield(&myState.rr.req)
									}
									return co.End()
								},
								func(co *async.Coroutine) async.Result {
									bytesToSend := myState.rr.req.data[sr.sent:]
									myRuntime.Go(func() {
										n, err := remote.Get().Write(bytesToSend)
										myRuntime.Spawn(async.Do(func() {
											result.n, result.err = n, err
											result.Notify()
										}))
									})
									return co.Await(&result).End()
								},
								func(co *async.Coroutine) async.Result {
									n, err := result.n, result.err
									if n > 0 {
										sr.sent += n
									}
									if err != nil {
										return co.Break()
									}
									return co.End()
								},
							)),
						))
					}
					stopSendingRequest := func(co *async.Coroutine) async.Result {
						aLongTimeAgo := time.Unix(1, 0)
						remote.Get().SetWriteDeadline(aLongTimeAgo)
						sr.stop.Set(true)
						return co.End()
					}
					awaitSendingRequest := sr.done.Await(untilTrue)
					sendRequest()
					var wgAfterFunc async.WaitGroup
					myRuntime.Add(1)
					wgAfterFunc.Add(1)
					stop := context.AfterFunc(ctx, func() {
						aLongTimeAgo := time.Unix(1, 0)
						remote.Get().SetReadDeadline(aLongTimeAgo)
						myRuntime.Spawn(async.Do(func() {
							myRuntime.Done()
							wgAfterFunc.Done()
						}))
					})
					stopAfterFunc := func(co *async.Coroutine) async.Result {
						if stop() {
							myRuntime.Done()
							wgAfterFunc.Done()
						}
						return co.End()
					}
					awaitAfterFunc := wgAfterFunc.Await()
					var resp struct {
						async.Signal
						buf [1]byte
						err error
					}
					myRuntime.Go(func() {
						for {
							n, err := remote.Get().Read(resp.buf[:])
							if n > 0 || err != nil {
								myRuntime.Spawn(async.Do(func() {
									resp.err = err
									resp.Notify()
								}))
								return
							}
						}
					})
					co.Defer(async.Block(
						stopSendingRequest,
						stopAfterFunc,
						awaitSendingRequest,
						awaitAfterFunc,
					))
					return co.Await(&resp).Then(
						func(co *async.Coroutine) async.Result {
							if resp.err != nil {
								return co.End()
							}
							if !timer.Stop() {
								return co.End()
							}
							if myState.hasWinner {
								return co.End()
							}
							myState.hasWinner = true
							thisIsTheWinner = true
							if lifeCycleCh != nil {
								lifeCycleCh <- ConnEventResponse{}
							}
							return co.Transition(async.Block(
								stopConnLoop,
								stopReadingRequest,
								stopSendingRequest,
								stopAfterFunc,
								awaitConnLoop,
								awaitReadingRequest,
								awaitSendingRequest,
								awaitAfterFunc,
								func(co *async.Coroutine) async.Result {
									l, r := pipeLeft, net.Conn(remote.Get())
									var noDeadline time.Time
									l.SetDeadline(noDeadline)
									r.SetDeadline(noDeadline)
									if sr.sent < len(myState.rr.req.data) {
										l = prefix(l, bytes.NewReader(myState.rr.req.data[sr.sent:]))
									}
									r = prefix(r, bytes.NewReader(resp.buf[:]))
									var sig async.Signal
									myRuntime.Go(func() {
										m.Relay(l, r, relayOpts)
										myRuntime.Spawn(async.Notify(&sig))
									})
									myConn.setOutgoingConn(remote.Get())
									return co.Await(&sig).Then(async.Do(func() {
										myConn.setOutgoingConn(nil)
										myState.sent = remote.Get().sent.Load()
										myState.recv = remote.Get().recv.Load()
										myState.remoteAddr = remote.Get().RemoteAddr()
									}))
								},
							))
						},
					)
				},
			)
		})
		return co.End()
	}

	startConnLoop := func() {
		var timer *time.Timer
		timerRunning := async.NewState(false)
		startTimer := func(co *async.Coroutine) async.Result {
			myState.cl.newconn.Set(false)
			timerRunning.Set(true)
			myRuntime.Add(1)
			if timer == nil {
				timer = time.AfterFunc(connOpts.Interval, func() {
					myRuntime.Spawn(async.Do(func() {
						myState.cl.newconn.Set(true)
						timerRunning.Set(false)
						myRuntime.Done()
					}))
				})
			} else {
				timer.Reset(connOpts.Interval)
			}
			return co.End()
		}
		stopTimer := func(co *async.Coroutine) async.Result {
			if timer != nil && timer.Stop() {
				myState.cl.newconn.Set(true)
				timerRunning.Set(false)
				myRuntime.Done()
			}
			return co.End()
		}
		awaitTimer := timerRunning.Await(untilFalse)
		myState.cl.newconn.Set(true)
		myRuntime.Spawn(async.Block(
			async.Defer(async.Block(
				stopTimer,
				awaitTimer,
				async.Do(func() { myState.cl.done.Set(true) }),
			)),
			async.Loop(async.Block(
				func(co *async.Coroutine) async.Result {
					if myState.cl.stop.Get() {
						return co.Break()
					}
					co.Watch(&myState.cl.stop)
					if !myState.cl.newconn.Get() {
						return co.Yield(&myState.cl.newconn)
					}
					if myState.cl.connections.n >= connOpts.Parallel {
						return co.Yield(&myState.cl.connections)
					}
					if myState.cl.attempts >= connOpts.MaxAttempts {
						if myState.cl.connections.n != 0 {
							return co.Yield(&myState.cl.connections)
						}
						if lifeCycleCh != nil {
							lifeCycleCh <- ConnEventMaxAttempts{}
						}
						return co.Break()
					}
					return co.End()
				},
				newConn,
				stopTimer,
				awaitTimer,
				startTimer,
			)),
		))
	}

	readRequest := func() {
		var result struct {
			async.Signal
			n   int
			err error
		}
		buf := connBufPool.Get().(*connBuf)
		myRuntime.Spawn(async.Block(
			async.Defer(async.Block(
				stopConnLoop,
				async.Do(func() {
					myState.rr.done.Set(true)
					connBufPool.Put(buf)
				}),
			)),
			async.Loop(async.Block(
				func(co *async.Coroutine) async.Result {
					myRuntime.Go(func() {
						n, err := pipeLeft.Read(buf[:])
						myRuntime.Spawn(async.Do(func() {
							result.n, result.err = n, err
							result.Notify()
						}))
					})
					return co.Await(&result).End()
				},
				func(co *async.Coroutine) async.Result {
					n, err := result.n, result.err
					if n > 0 {
						if myState.rr.req.data == nil {
							reqBuf = connBufPool.Get().(*connBuf)
							myState.rr.req.data = reqBuf[:0]
						}
						myState.rr.req.data = append(myState.rr.req.data, buf[:n]...)
						myState.rr.req.Notify()
					}
					if err != nil {
						if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrDeadlineExceeded) {
							logger.Debug("conn:readreq", slog.Any("error", err))
						}
						return co.Break()
					}
					if len(myState.rr.req.data) >= maxRequestSize {
						logger.Debug("conn:readreq", slog.String("error", "request too large"))
						return co.Break()
					}
					return co.End()
				},
			)),
		))
	}

	logger.Debug("conn:open")

	if lifeCycleCh != nil {
		go lifeCycle(lifeCycleCh)
	}

	readRequest()
	startConnLoop()

	go func() {
		myRuntime.Wait()
		pipeLeft.Close()
		if lifeCycleCh != nil {
			close(lifeCycleCh)
		}
		if reqBuf != nil {
			connBufPool.Put(reqBuf)
		}
		if myState.hasWinner {
			logger.Debug("conn:close",
				slog.Int("reqsz", len(myState.rr.req.data)),
				slog.Int64("sent", myState.sent),
				slog.Int64("recv", myState.recv),
				slog.String("from", myState.remoteAddr.String()))
		} else {
			logger.Debug("conn:close", slog.Int("reqsz", len(myState.rr.req.data)))
		}
	}()

	return myConn
}

type Conn struct {
	net.Conn

	mu     sync.Mutex
	oc     net.Conn
	tr, tw time.Duration
}

func toDeadline(d time.Duration) time.Time {
	if d == 0 {
		return time.Time{}
	}
	return time.Now().Add(d)
}

func (c *Conn) setOutgoingConn(oc net.Conn) {
	c.mu.Lock()
	c.oc = oc
	if oc != nil {
		oc.SetReadDeadline(toDeadline(c.tr))
		oc.SetWriteDeadline(toDeadline(c.tw))
	}
	c.mu.Unlock()
}

func (c *Conn) SetOutgoingTimeout(tr, tw time.Duration) {
	c.mu.Lock()
	c.tr, c.tw = tr, tw
	if oc := c.oc; oc != nil {
		oc.SetReadDeadline(toDeadline(tr))
		oc.SetWriteDeadline(toDeadline(tw))
	}
	c.mu.Unlock()
}

func prefix(c net.Conn, r io.Reader) net.Conn {
	type A = struct {
		net.Conn
		io.Reader
	}
	type B = struct {
		A
		io.Reader
	}
	return &B{A{c, nil}, io.MultiReader(r, c)}
}

func untilTrue(v bool) bool { return v }

func untilFalse(v bool) bool { return !v }

type conn struct {
	net.Conn
	sent, recv atomic.Int64
}

func (c *conn) Read(b []byte) (n int, err error) {
	n, err = c.Conn.Read(b)
	if n > 0 {
		c.recv.Add(int64(n))
	}
	return
}

func (c *conn) Write(b []byte) (n int, err error) {
	n, err = c.Conn.Write(b)
	if n > 0 {
		c.sent.Add(int64(n))
	}
	return
}

type closeConnError struct{}

func (closeConnError) Error() string { return "close conn" }

func (closeConnError) Is(err error) bool { return err == context.Canceled }

var CloseConn error = closeConnError{}

type connBuf = [8 << 10]byte

var connBufPool = sync.Pool{
	New: func() any { return new(connBuf) },
}

const (
	defaultConnTimeout     = 10 * time.Second
	defaultConnInterval    = 2 * time.Second
	defaultConnParallel    = 1
	defaultConnMaxAttempts = 30
	maxRequestSize         = 32 << 10
)
