package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"
)

func init() {
	proxy.RegisterDialerType("ws", wsFromURL)
	proxy.RegisterDialerType("wss", wsFromURL)
}

func wsFromURL(u *url.URL, forward proxy.Dialer) (proxy.Dialer, error) {
	return &wsDialer{u, forward}, nil
}

type wsDialer struct {
	URL     *url.URL
	Forward proxy.Dialer
}

func (d *wsDialer) Dial(network, addr string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, addr)
}

func (d *wsDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return d.dialTCP(ctx, network, addr)
	default:
		return nil, net.UnknownNetworkError(network)
	}
}

func (d *wsDialer) dialTCP(ctx context.Context, _, addr string) (net.Conn, error) {
	dialer := &websocket.Dialer{
		NetDialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return Dial(ctx, d.Forward, network, addr) // Use addr from dialTCP.
		},
		HandshakeTimeout: 45 * time.Second,
	}

	u := d.URL
	if u.Host == "" || u.Path == "" {
		copy := *d.URL
		u = &copy

		if u.Host == "" {
			u.Host = addr
		}

		if u.Path == "" {
			u.Path = "/"
		}
	}

	conn, resp, err := dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("proxy/websocket: dial %v: %w (%v)", u.String(), err, resp.Status)
		}

		return nil, fmt.Errorf("proxy/websocket: dial %v: %w", u.String(), err)
	}

	return &wsConnection{ws: conn}, nil
}

type wsConnection struct {
	ws     *websocket.Conn
	reader io.Reader
}

func (c *wsConnection) Read(b []byte) (n int, err error) {
	for {
		if c.reader == nil {
			_, reader, err := c.ws.NextReader()
			if err != nil {
				if err != io.EOF { // Should not wrap io.EOF.
					err = fmt.Errorf("proxy/websocket: read: %w", err)
				}

				return 0, err
			}

			c.reader = reader
		}

		n, err := c.reader.Read(b)
		if err == io.EOF {
			c.reader = nil
			continue
		}

		return n, fmt.Errorf("proxy/websocket: read: %w", err)
	}
}

func (c *wsConnection) Write(b []byte) (n int, err error) {
	err = c.ws.WriteMessage(websocket.BinaryMessage, b)
	if err != nil {
		err = fmt.Errorf("proxy/websocket: write: %w", err)
		return
	}

	return len(b), nil
}

func (c *wsConnection) Close() error {
	return c.ws.Close()
}

func (c *wsConnection) LocalAddr() net.Addr {
	return c.ws.LocalAddr()
}

func (c *wsConnection) RemoteAddr() net.Addr {
	return c.ws.RemoteAddr()
}

func (c *wsConnection) SetDeadline(t time.Time) error {
	err := c.SetReadDeadline(t)
	if err != nil {
		return err
	}

	return c.SetWriteDeadline(t)
}

func (c *wsConnection) SetReadDeadline(t time.Time) error {
	return c.ws.SetReadDeadline(t)
}

func (c *wsConnection) SetWriteDeadline(t time.Time) error {
	return c.ws.SetWriteDeadline(t)
}
