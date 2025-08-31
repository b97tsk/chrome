package chrome

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/b97tsk/async"
	"github.com/b97tsk/log"
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
	logger *log.Logger,
	lifeCycle func(c <-chan ConnEvent),
) net.Conn {
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

	var wg sync.WaitGroup

	var myExecutor async.Executor

	myExecutor.Autorun(func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			myExecutor.Run()
		}()
	})

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
	}

	var lifeCycleCh chan ConnEvent

	pipeLeft, pipeRight := net.Pipe()

	stopConnLoop := func(co *async.Coroutine) async.Result {
		myState.cl.stop.Set(true)
		return co.End()
	}

	untilTrue := func(v bool) bool { return v }

	awaitConnLoop := myState.cl.done.Await(untilTrue)

	stopReadingRequest := func(co *async.Coroutine) async.Result {
		aLongTimeAgo := time.Unix(1, 0)
		pipeLeft.SetReadDeadline(aLongTimeAgo)
		myState.rr.stop.Set(true)
		return co.End()
	}

	awaitReadingRequest := myState.rr.done.Await(untilTrue)

	hasWinner := false

	newConn := func(co *async.Coroutine) async.Result {
		myState.cl.connections.n++
		myState.cl.attempts++
		thisIsTheWinner := false
		ctx, cancel := context.WithCancel(context.Background())
		myExecutor.Spawn(func(co *async.Coroutine) async.Result {
			var sig async.Signal
			wg.Add(1)
			stop := context.AfterFunc(ctx, func() {
				myExecutor.Spawn(async.Do(func() {
					sig.Notify()
					wg.Done()
				}))
			})
			co.CleanupFunc(func() {
				if stop() {
					wg.Done()
				}
			})
			co.Spawn(async.Block(
				awaitConnLoop,
				async.Do(func() {
					if !thisIsTheWinner {
						cancel()
					}
					sig.Notify()
				}),
			))
			return co.Await(&sig).End()
		})
		myExecutor.Spawn(func(co *async.Coroutine) async.Result {
			timer := time.AfterFunc(connOpts.Timeout, cancel)
			var remote async.State[net.Conn]
			wg.Add(1)
			go func() {
				defer wg.Done()
				conn, err := getRemote(ctx)
				myExecutor.Spawn(func(co *async.Coroutine) async.Result {
					remote.Set(conn)
					if errors.Is(err, CloseConn) {
						return co.Transition(stopReadingRequest)
					}
					return co.End()
				})
				if err != nil && !errors.Is(err, context.Canceled) && logger != nil {
					logger.Tracef("connect(%p): %v", &wg, err)
				}
			}()
			co.Defer(
				async.Do(func() {
					if conn := remote.Get(); conn != nil {
						conn.Close()
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
						myExecutor.Spawn(async.Block(
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
									wg.Add(1)
									go func(bytesToSend []byte) {
										defer wg.Done()
										n, err := remote.Get().Write(bytesToSend)
										myExecutor.Spawn(async.Do(func() {
											result.n, result.err = n, err
											result.Notify()
										}))
									}(myState.rr.req.data[sr.sent:])
									return co.Transition(async.Await(&result))
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
					wg.Add(1)
					wgAfterFunc.Add(1)
					stop := context.AfterFunc(ctx, func() {
						aLongTimeAgo := time.Unix(1, 0)
						remote.Get().SetReadDeadline(aLongTimeAgo)
						myExecutor.Spawn(async.Do(func() {
							wg.Done()
							wgAfterFunc.Done()
						}))
					})
					stopAfterFunc := func(co *async.Coroutine) async.Result {
						if stop() {
							wg.Done()
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
					wg.Add(1)
					go func() {
						defer wg.Done()
						for {
							n, err := remote.Get().Read(resp.buf[:])
							if n > 0 || err != nil {
								err := err
								myExecutor.Spawn(async.Do(func() {
									resp.err = err
									resp.Notify()
								}))
								return
							}
						}
					}()
					co.Defer(async.Block(
						stopSendingRequest,
						awaitSendingRequest,
						stopAfterFunc,
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
							if hasWinner {
								return co.End()
							}
							hasWinner = true
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
									l, r := pipeLeft, remote.Get()
									var noDeadline time.Time
									l.SetDeadline(noDeadline)
									r.SetDeadline(noDeadline)
									if sr.sent < len(myState.rr.req.data) {
										l = mixin(l, io.MultiReader(bytes.NewReader(myState.rr.req.data[sr.sent:]), l))
									}
									r = mixin(r, io.MultiReader(bytes.NewReader(resp.buf[:]), r))
									var sig async.Signal
									wg.Add(1)
									go func() {
										defer wg.Done()
										m.Relay(l, r, relayOpts)
										myExecutor.Spawn(async.Do(sig.Notify))
									}()
									return co.Await(&sig).End()
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
			wg.Add(1)
			if timer == nil {
				timer = time.AfterFunc(connOpts.Interval, func() {
					myExecutor.Spawn(async.Do(func() {
						myState.cl.newconn.Set(true)
						timerRunning.Set(false)
						wg.Done()
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
				wg.Done()
			}
			return co.End()
		}
		awaitTimer := func(co *async.Coroutine) async.Result {
			if timerRunning.Get() {
				return co.Yield(timerRunning)
			}
			return co.End()
		}
		myState.cl.newconn.Set(true)
		myExecutor.Spawn(async.Block(
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
		var buf [4 << 10]byte
		var result struct {
			async.Signal
			n   int
			err error
		}
		myExecutor.Spawn(async.Block(
			async.Defer(async.Block(
				stopConnLoop,
				async.Do(func() { myState.rr.done.Set(true) }),
			)),
			async.Loop(async.Block(
				func(co *async.Coroutine) async.Result {
					wg.Add(1)
					go func() {
						defer wg.Done()
						n, err := pipeLeft.Read(buf[:])
						myExecutor.Spawn(async.Do(func() {
							result.n, result.err = n, err
							result.Notify()
						}))
					}()
					return co.Await(&result).End()
				},
				func(co *async.Coroutine) async.Result {
					n, err := result.n, result.err
					if n > 0 {
						myState.rr.req.data = append(myState.rr.req.data, buf[:n]...)
						myState.rr.req.Notify()
					}
					if err != nil {
						return co.Break()
					}
					return co.End()
				},
			)),
		))
	}

	if logger != nil {
		logger.Tracef("connect(%p): %v", &wg, remoteAddr)
	}

	if lifeCycle != nil {
		lifeCycleCh = make(chan ConnEvent)
		go lifeCycle(lifeCycleCh)
	}

	readRequest()
	startConnLoop()

	go func() {
		wg.Wait()
		pipeLeft.Close()
		if lifeCycleCh != nil {
			close(lifeCycleCh)
		}
		if logger != nil {
			logger.Tracef("connect(%p): %v (CLOSED)", &wg, remoteAddr)
		}
	}()

	return pipeRight
}

func mixin(c net.Conn, r io.Reader) net.Conn {
	type A = struct {
		net.Conn
		io.Reader
	}
	type B = struct {
		A
		io.Reader
	}
	return &B{A{c, nil}, r}
}

type closeConnError struct{}

func (closeConnError) Error() string { return "close conn" }

func (closeConnError) Is(err error) bool { return err == context.Canceled }

var CloseConn error = closeConnError{}

const (
	defaultConnTimeout     = 10 * time.Second
	defaultConnInterval    = 2 * time.Second
	defaultConnParallel    = 1
	defaultConnMaxAttempts = 30
)
