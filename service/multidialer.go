package service

import (
	"context"
	"net"
	"sync"
	"time"
)

const (
	numTries      = 5
	dialInterval  = 3 * time.Second
	dialTimeout   = 6 * time.Second
	dialKeepAlive = 30 * time.Second
	poolTimeout   = 2 * time.Second
)

type MultiDialer struct {
	mu    sync.Mutex
	idles sync.Map
}

func (d *MultiDialer) Dial(network, addr string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, addr)
}

func (d *MultiDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return d.connect(ctx, network, addr)
	default:
		return nil, net.UnknownNetworkError(network)
	}
}

func (d *MultiDialer) connect(ctx context.Context, network, addr string) (net.Conn, error) {
	type Result struct {
		Conn net.Conn
		Err  error
	}
	result := make(chan Result, numTries)
	resultCount := 0
	expectCount := numTries
	defer func() {
		if resultCount < expectCount {
			go func() {
				var cp *connPool
				for resultCount < expectCount {
					r := <-result
					resultCount++
					if r.Err == nil {
						if cp == nil {
							d.mu.Lock()
							if v, ok := d.idles.Load(addr); ok {
								cp = v.(*connPool)
							} else {
								cp = new(connPool)
								d.idles.Store(addr, cp)
							}
							d.mu.Unlock()
						}
						cp.Put(r.Conn)
					}
				}
			}()
		}
	}()
	direct := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: dialKeepAlive,
	}
	connect := func() {
		if v, ok := d.idles.Load(addr); ok {
			conn := v.(*connPool).Get()
			if conn != nil {
				result <- Result{conn, nil}
				return
			}
		}
		conn, err := direct.DialContext(ctx, network, addr)
		result <- Result{conn, err}
	}
	go connect()
	done := ctx.Done()
	timer := time.NewTimer(dialInterval)
	for iTries := 1; iTries < numTries; {
		select {
		case <-timer.C:
			go connect()
			iTries++
			if iTries < numTries {
				timer.Reset(dialInterval)
			}
		case r := <-result:
			resultCount++
			if r.Err == nil {
				expectCount = iTries
				return r.Conn, r.Err
			}
		case <-done:
			expectCount = iTries
			return nil, ctx.Err()
		}
	}
	for {
		select {
		case r := <-result:
			resultCount++
			if r.Err == nil || resultCount == expectCount {
				return r.Conn, r.Err
			}
		case <-done:
			return nil, ctx.Err()
		}
	}
}

type connPool struct {
	mu    sync.Mutex
	conns []net.Conn
}

func (p *connPool) Get() net.Conn {
	var conn net.Conn
	p.mu.Lock()
	if len(p.conns) > 0 {
		conn, p.conns = p.conns[0], p.conns[1:]
	}
	p.mu.Unlock()
	return conn
}

func (p *connPool) Put(conn net.Conn) {
	p.mu.Lock()
	p.conns = append(p.conns, conn)
	p.mu.Unlock()
	time.AfterFunc(poolTimeout, func() {
		shouldClose := false
		p.mu.Lock()
		for i, c := range p.conns {
			if c == conn {
				copy(p.conns[i:], p.conns[i+1:])
				last := len(p.conns) - 1
				p.conns[last] = nil
				p.conns = p.conns[:last]
				shouldClose = true
				break
			}
		}
		p.mu.Unlock()
		if shouldClose {
			conn.Close()
		}
	})
}
