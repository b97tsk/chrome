package failover

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net"
	"slices"
	"sort"
	"sync"

	"github.com/b97tsk/chrome"
	"github.com/b97tsk/chrome/proxy"
	"github.com/shadowsocks/go-shadowsocks2/socks"
)

type Options struct {
	ListenAddr string `yaml:"on"`

	Candidates []chrome.Proxy

	Conn  chrome.ConnOptions
	Relay chrome.RelayOptions

	queue *queue
}

type queue struct {
	mu         sync.Mutex
	elems      []*elem
	recent     *elem
	nLowScore  int // Number of elems that have low score (lower than maxScore/2).
	nHighScore int // Number of elems that have high score (higher than maxScore/2).
}

func newQueue(n int) *queue {
	elems := make([]*elem, n)
	for i := range elems {
		elems[i] = newElem(i)
	}
	return &queue{elems: elems}
}

func (q *queue) pick() (e *elem, sameold bool) {
	q.mu.Lock()
	s := q.elems
	e = s[0]
	n := sort.Search(len(s[1:]), func(i int) bool { return s[i+1].Score < e.Score }) + 1
	e = s[rand.IntN(n)]
	recent := q.recent
	q.recent = e
	q.mu.Unlock()
	return e, e == recent
}

func (q *queue) succ(e *elem) { q.fix(e, true) }
func (q *queue) fail(e *elem) { q.fix(e, false) }

func (q *queue) fix(e *elem, success bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	oldScore := e.Score

	if success {
		e.succ()
	} else {
		e.fail()
	}

	if e.Score == oldScore {
		return
	}

	s := q.elems
	i := slices.Index(s, e)
	for j := i; j < len(s)-1; j++ {
		if s[j].Score >= s[j+1].Score {
			break
		}
		s[j], s[j+1] = s[j+1], s[j]
	}
	for j := i; j > 0; j-- {
		if s[j].Score <= s[j-1].Score {
			break
		}
		s[j], s[j-1] = s[j-1], s[j]
	}

	q.scoreChanged(oldScore, e.Score)

	if q.nLowScore < len(s) && q.nHighScore < len(s) {
		return
	}

	totalScore := 0
	for _, e := range s {
		totalScore += e.Score
	}

	offset := maxScore/2 - totalScore/len(s)
	for _, e := range s {
		oldScore := e.Score
		e.Score += offset
		q.scoreChanged(oldScore, e.Score)
	}
}

func (q *queue) scoreChanged(oldScore, newScore int) {
	switch {
	case oldScore < maxScore/2:
		switch {
		case newScore > maxScore/2:
			q.nLowScore--
			q.nHighScore++
		case newScore == maxScore/2:
			q.nLowScore--
		}
	case oldScore > maxScore/2:
		switch {
		case newScore < maxScore/2:
			q.nLowScore++
			q.nHighScore--
		case newScore == maxScore/2:
			q.nHighScore--
		}
	default:
		switch {
		case newScore < maxScore/2:
			q.nLowScore++
		case newScore > maxScore/2:
			q.nHighScore++
		}
	}
}

type elem struct {
	Index int
	Score int
	N     int // Number of consecutive successes or failures.
}

func newElem(i int) *elem {
	return &elem{
		Index: i,
		Score: maxScore / 2,
	}
}

func (e *elem) Less(other *elem) bool {
	if d := e.Score - other.Score; d != 0 {
		return d > 0
	}
	return e.Index < other.Index
}

func (e *elem) succ() {
	if e.Score == maxScore {
		return
	}
	if e.N < 0 {
		e.N = 0
	}
	if e.N < maxN {
		e.N++
	}
	e.Score += fib(e.N)
	if e.Score > maxScore {
		e.Score = maxScore
	}
}

func (e *elem) fail() {
	if e.Score == 0 {
		return
	}
	if e.N > 0 {
		e.N = 0
	}
	if e.N > -maxN {
		e.N--
	}
	e.Score -= fib(-e.N)
	if e.Score < 0 {
		e.Score = 0
	}
}

type Service struct{}

const ServiceName = "failover"

func (Service) Name() string {
	return ServiceName
}

func (Service) Options() any {
	return new(Options)
}

func (Service) Run(ctx chrome.Context) {
	logger := ctx.Manager.Logger().With(slog.String("job", ctx.JobName))

	optsIn, optsOut := make(chan Options), make(chan Options)
	defer close(optsIn)

	go func() {
		var opts Options

		ok := true
		for ok {
			select {
			case opts, ok = <-optsIn:
			case optsOut <- opts:
			}
		}

		close(optsOut)
	}()

	var server net.Listener

	startServer := func() error {
		if server != nil {
			return nil
		}

		ln, err := net.Listen("tcp", (<-optsOut).ListenAddr)
		if err != nil {
			logger.Error("net:listen", slog.Any("error", err))
			return err
		}

		defer logger.Info("net:listening", slog.Any("addr", ln.Addr()))

		server = ln

		go ctx.Manager.Serve(ln, func(local net.Conn) {
			addr, err := socks.Handshake(local)
			if err != nil {
				return
			}

			opts, ok := <-optsOut
			if !ok {
				return
			}

			remoteAddr := addr.String()

			getRemote := func(ctx context.Context) (net.Conn, error) {
				opts := <-optsOut
				q := opts.queue
				if q == nil {
					return nil, chrome.CloseConn
				}
				e, sameold := q.pick()
				if !sameold {
					logger.Debug("pick", slog.Int("index", e.Index))
				}
				c, err := proxy.Dial(ctx, opts.Candidates[e.Index].Dialer(), "tcp", remoteAddr)
				if err != nil {
					q.fail(e)
					return nil, err
				}
				return newConn(c, q, e), nil
			}

			remote := ctx.Manager.NewConn(remoteAddr, getRemote, opts.Conn, opts.Relay, logger, nil)
			defer remote.Close()

			ctx.Manager.Relay(local, remote, opts.Relay)
		})

		return nil
	}

	stopServer := func() {
		if server == nil {
			return
		}

		defer logger.Info("net:listen:close", slog.Any("addr", server.Addr()))

		_ = server.Close()
		server = nil
	}
	defer stopServer()

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ctx.Event:
			switch ev := ev.(type) {
			case chrome.LoadEvent:
				old := <-optsOut
				new := *ev.Options.(*Options)

				if _, _, err := net.SplitHostPort(new.ListenAddr); err != nil {
					logger.Error("loading", slog.Any("error", err))
					return
				}

				if n := len(new.Candidates); n != 0 {
					if slices.EqualFunc(new.Candidates, old.Candidates, chrome.Proxy.Equal) {
						new.queue = old.queue
					} else {
						new.queue = newQueue(n)
					}
				}

				if new.ListenAddr != old.ListenAddr {
					stopServer()
				}

				optsIn <- new
			case chrome.LoadedEvent:
				if err := startServer(); err != nil {
					return
				}
			}
		}
	}
}

type conn struct {
	net.Conn

	q *queue
	e *elem

	success    bool
	failure    bool
	readfailed bool
}

func newConn(c net.Conn, q *queue, e *elem) *conn {
	return &conn{Conn: c, q: q, e: e}
}

func (c *conn) Read(b []byte) (n int, err error) {
	n, err = c.Conn.Read(b)
	if n > 0 && !c.success {
		c.success = true
		c.q.succ(c.e)
	}
	if err != nil {
		c.readfailed = true
	}
	return
}

func (c *conn) Close() error {
	if !c.success && !c.failure && c.readfailed {
		c.failure = true
		c.q.fail(c.e)
	}
	return c.Conn.Close()
}

func fib(n int) int {
	for i, j := 0, 1; ; i, j, n = j, i+j, n-1 {
		if n < 1 {
			return i
		}
	}
}

const (
	maxScore = 64
	maxN     = 9 // fib(9) = 34
)
