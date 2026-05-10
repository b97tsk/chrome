package proxy

import (
	"context"
	"fmt"
	"net"
	"net/url"
)

func init() {
	proxy_RegisterDialerType("tcptun", tcptunFromURL)
}

func tcptunFromURL(u *url.URL, forward proxy_Dialer) (proxy_Dialer, error) {
	return &tcptunDialer{u.Host, u.Port() != "", forward}, nil
}

type tcptunDialer struct {
	Server  string
	HasPort bool
	Forward proxy_Dialer
}

func (d *tcptunDialer) Dial(network, addr string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, addr)
}

func (d *tcptunDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("proxy/tcptun: network not implemented: %v", network)
	}

	target := d.Server
	if !d.HasPort {
		_, port, _ := net.SplitHostPort(addr)
		target = net.JoinHostPort(target, port)
	}

	c, err := Dial(ctx, d.Forward, network, target)
	if err != nil {
		err = fmt.Errorf("proxy/tcptun: dial %v: %w", d.Server, err)
	}

	return c, err
}
