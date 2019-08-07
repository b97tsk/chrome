package proxy

import (
	"context"
	"net"

	"golang.org/x/net/proxy"
)

func Dial(ctx context.Context, d proxy.Dialer, network, address string) (net.Conn, error) {
	if xd, ok := d.(proxy.ContextDialer); ok {
		return xd.DialContext(ctx, network, address)
	}
	type Result struct {
		Conn net.Conn
		Err  error
	}
	result := make(chan Result, 1)
	go func() {
		conn, err := d.Dial(network, address)
		result <- Result{conn, err}
	}()
	select {
	case <-ctx.Done():
		go func() {
			r := <-result
			if r.Conn != nil {
				r.Conn.Close()
			}
		}()
		return nil, ctx.Err()
	case r := <-result:
		return r.Conn, r.Err
	}
}
