package proxy

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/time/rate"
)

const rateLimitBurst = 32 << 10

func init() {
	proxy_RegisterDialerType("ratelimit", rateLimitFromURL)
}

func rateLimitFromURL(u *url.URL, forward proxy_Dialer) (proxy_Dialer, error) {
	values := u.Query()
	r := rateLimitParseRate(values, "r", "read", "rw", "readwrite")
	w := rateLimitParseRate(values, "w", "write", "rw", "readwrite")
	return &rateLimitDialer{r, w, forward}, nil
}

func rateLimitParseRate(values url.Values, keys ...string) int {
	for _, key := range keys {
		s := values.Get(key)
		if s == "" {
			continue
		}
		n := 0
		switch s[len(s)-1] {
		case 'K', 'k':
			n = 10
		case 'M', 'm':
			n = 20
		case 'G', 'g':
			n = 30
		}
		s = strings.TrimRight(s, "KkMmGg")
		i, err := strconv.Atoi(s)
		if err != nil {
			return 0
		}
		return i << n
	}
	return 0
}

type rateLimitDialer struct {
	ReadRate  int
	WriteRate int
	Forward   proxy_Dialer
}

func (d *rateLimitDialer) Dial(network, addr string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, addr)
}

func (d *rateLimitDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("proxy/ratelimit: network not implemented: %v", network)
	}

	c, err := Dial(ctx, d.Forward, network, addr)
	if err != nil {
		err = fmt.Errorf("proxy/ratelimit: dial %v: %w", addr, err)
	}

	if err == nil && (d.ReadRate > 0 || d.WriteRate > 0) {
		l := &rateLimiter{Conn: c}
		if d.ReadRate > 0 {
			l.r = rate.NewLimiter(rate.Limit(d.ReadRate), rateLimitBurst)
		}
		if d.WriteRate > 0 {
			l.w = rate.NewLimiter(rate.Limit(d.WriteRate), rateLimitBurst)
		}
		c = l
	}

	return c, err
}

type rateLimiter struct {
	net.Conn
	r, w *rate.Limiter
}

func (l *rateLimiter) Read(b []byte) (n int, err error) {
	n, err = l.Conn.Read(b)
	if l.r != nil && err == nil {
		if l.r.Burst() < len(b) {
			l.r.SetBurst(len(b))
		}
		err = l.r.WaitN(context.Background(), n)
	}
	return
}

func (l *rateLimiter) Write(b []byte) (n int, err error) {
	n, err = l.Conn.Write(b)
	if l.w != nil && err == nil {
		if l.w.Burst() < len(b) {
			l.w.SetBurst(len(b))
		}
		err = l.w.WaitN(context.Background(), n)
	}
	return
}
