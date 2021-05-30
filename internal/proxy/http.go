package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/proxy"
)

func init() {
	proxy.RegisterDialerType("http", httpFromURL)
}

func httpFromURL(u *url.URL, forward proxy.Dialer) (proxy.Dialer, error) {
	var auth *proxy.Auth
	if u.User != nil {
		auth = new(proxy.Auth)
		auth.User = u.User.Username()

		if p, ok := u.User.Password(); ok {
			auth.Password = p
		}
	}

	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(host, "80")
	}

	return &httpDialer{host, auth, forward}, nil
}

type httpDialer struct {
	Server  string
	Auth    *proxy.Auth
	Forward proxy.Dialer
}

func (d *httpDialer) Dial(network, addr string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, addr)
}

func (d *httpDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return d.dialTCP(ctx, network, addr)
	default:
		return nil, net.UnknownNetworkError(network)
	}
}

func (d *httpDialer) dialTCP(ctx context.Context, network, addr string) (c net.Conn, err error) {
	c, err = Dial(ctx, d.Forward, network, d.Server)
	if err != nil {
		return nil, fmt.Errorf("proxy/http: dial %v: %w", d.Server, err)
	}

	defer func() {
		if c != nil {
			var noDeadline time.Time
			_ = c.SetDeadline(noDeadline)
		}
	}()

	if d, ok := ctx.Deadline(); ok && !d.IsZero() {
		_ = c.SetDeadline(d)
	}

	if ctx.Done() != nil {
		watch := make(chan struct{})
		done := make(chan struct{})

		defer func() {
			close(done)

			if err == nil {
				<-watch
			}
		}()

		go func(c net.Conn) {
			defer close(watch)
			select {
			case <-done:
			case <-ctx.Done():
				aLongTimeAgo := time.Unix(1, 0)
				_ = c.SetDeadline(aLongTimeAgo)
			}
		}(c)
	}

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: http.Header{
			"User-Agent": []string(nil),
		},
	}

	if d.Auth != nil {
		authString := d.Auth.User + ":" + d.Auth.Password
		authString = base64.StdEncoding.EncodeToString([]byte(authString))
		req.Header.Set("Proxy-Authorization", "Basic "+authString)
	}

	if err := req.Write(c); err != nil {
		c.Close()

		return nil, fmt.Errorf("proxy/http: dial %v over %v: %w", addr, d.Server, err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(c), req)
	if err != nil {
		c.Close()

		return nil, fmt.Errorf("proxy/http: dial %v over %v: %w", addr, d.Server, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.Close()

		return nil, fmt.Errorf("proxy/http: dial %v over %v: %v", addr, d.Server, resp.Status)
	}

	return c, nil
}
