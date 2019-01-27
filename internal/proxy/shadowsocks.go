package proxy

import (
	"net"
	"net/url"
	"strings"

	"github.com/b97tsk/chrome/internal/utility"
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
		bytes, err := utility.DecodeBase64String(u.Host)
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
		bytes, err := utility.DecodeBase64String(method)
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
	return shadowsocksDialer{u.Host, cipher, forward}, nil
}

type shadowsocksDialer struct {
	Server  string
	Cipher  core.Cipher
	Forward proxy.Dialer
}

func (d shadowsocksDialer) Dial(network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return d.DialTCP(network, addr)
	default:
		return nil, net.UnknownNetworkError(network)
	}
}

func (d shadowsocksDialer) DialTCP(network, addr string) (net.Conn, error) {
	remoteAddr := socks.ParseAddr(addr)
	if remoteAddr == nil {
		return nil, shadowsocksParseAddrError(addr)
	}
	conn, err := d.Forward.Dial("tcp", d.Server)
	if err != nil {
		return nil, err
	}
	conn = d.Cipher.StreamConn(conn)
	_, err = conn.Write(remoteAddr)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

type shadowsocksParseAddrError string

func (e shadowsocksParseAddrError) Error() string {
	return "invalid addr: " + string(e)
}

type shadowsocksUnknownSSError struct {
	u *url.URL
}

func (e shadowsocksUnknownSSError) Error() string {
	return "unknown ss: " + e.u.String()
}

type shadowsocksUnknownCipherError struct {
	u *url.URL
}

func (e shadowsocksUnknownCipherError) Error() string {
	return "unknown cipher: " + e.u.String()
}
