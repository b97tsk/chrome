package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/shadowsocks/go-shadowsocks2/core"
	"github.com/shadowsocks/go-shadowsocks2/socks"
	"golang.org/x/net/proxy"
)

func init() {
	proxy.RegisterDialerType("ss", shadowsocksFromURL)
}

func shadowsocksFromURL(u *url.URL, forward proxy.Dialer) (proxy.Dialer, error) {
	origin := u

	if u.User == nil {
		bytes, err := decodeBase64String(u.Host)
		if err != nil {
			return nil, shadowsocksUnknownSSError{origin}
		}

		u, _ = url.Parse(u.Scheme + "://" + string(bytes))
		if u == nil || u.User == nil {
			return nil, shadowsocksUnknownSSError{origin}
		}
	}

	method := u.User.Username()

	password, ok := u.User.Password()
	if !ok {
		bytes, err := decodeBase64String(method)
		if err != nil {
			return nil, shadowsocksUnknownSSError{origin}
		}

		slice := strings.SplitN(string(bytes), ":", 2)
		if len(slice) != 2 {
			return nil, shadowsocksUnknownSSError{origin}
		}

		method, password = slice[0], slice[1]
	}

	cipher, err := core.PickCipher(method, nil, password)
	if err != nil {
		return nil, shadowsocksUnknownCipherError{origin}
	}

	return &shadowsocksDialer{u.Host, cipher, forward}, nil
}

func decodeBase64String(s string) ([]byte, error) {
	s = strings.ReplaceAll(s, "-", "+")
	s = strings.ReplaceAll(s, "_", "/")

	enc := base64.StdEncoding
	if len(s)%4 != 0 {
		enc = base64.RawStdEncoding
	}

	return enc.DecodeString(s)
}

type shadowsocksDialer struct {
	Server  string
	Cipher  core.Cipher
	Forward proxy.Dialer
}

func (d *shadowsocksDialer) Dial(network, addr string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, addr)
}

func (d *shadowsocksDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return d.dialTCP(ctx, network, addr)
	default:
		return nil, net.UnknownNetworkError(network)
	}
}

func (d *shadowsocksDialer) dialTCP(ctx context.Context, network, addr string) (net.Conn, error) {
	remoteAddr := socks.ParseAddr(addr)
	if remoteAddr == nil {
		return nil, shadowsocksParseAddrError(addr)
	}

	conn, err := Dial(ctx, d.Forward, network, d.Server)
	if err != nil {
		return nil, fmt.Errorf("proxy/shadowsocks: dial %v: %w", d.Server, err)
	}

	conn = d.Cipher.StreamConn(conn)

	_, err = conn.Write(remoteAddr)
	if err != nil {
		conn.Close()

		return nil, fmt.Errorf("proxy/shadowsocks: write %v: %w", addr, err)
	}

	return conn, nil
}

type shadowsocksParseAddrError string

func (e shadowsocksParseAddrError) Error() string {
	return "proxy/shadowsocks: parse addr: " + string(e)
}

type shadowsocksUnknownSSError struct {
	u *url.URL
}

func (e shadowsocksUnknownSSError) Error() string {
	return "proxy/shadowsocks: unknown ss: " + e.u.String()
}

type shadowsocksUnknownCipherError struct {
	u *url.URL
}

func (e shadowsocksUnknownCipherError) Error() string {
	return "proxy/shadowsocks: unknown cipher: " + e.u.String()
}
