package main

import (
	"net"
	"net/url"
	"strings"

	"github.com/shadowsocks/go-shadowsocks2/core"
	"github.com/shadowsocks/go-shadowsocks2/socks"
	"golang.org/x/net/proxy"
)

type sslocalDialer struct {
	Server  string
	Cipher  core.Cipher
	Forward proxy.Dialer
}

func (d sslocalDialer) Dial(network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return d.DialTCP(network, addr)
	default:
		return nil, net.UnknownNetworkError(network)
	}
}

func (d sslocalDialer) DialTCP(network, addr string) (c net.Conn, err error) {
	remoteAddr := socks.ParseAddr(addr)
	if remoteAddr == nil {
		err = sslocalParseAddrError(addr)
		return
	}
	c, err = d.Forward.Dial("tcp", d.Server)
	if err != nil {
		return
	}
	c = d.Cipher.StreamConn(c)
	_, err = c.Write(remoteAddr)
	if err != nil {
		c.Close()
		c = nil
	}
	return
}

func sslocalFromURL(u *url.URL, forward proxy.Dialer) (proxy.Dialer, error) {
	origin := u
	if u.User == nil {
		bytes, err := decodeBase64String(u.Host)
		if err != nil {
			return nil, sslocalUnknownSSError{origin}
		}
		u, _ = url.Parse(u.Scheme + "://" + string(bytes))
		if u == nil || u.User == nil {
			return nil, sslocalUnknownSSError{origin}
		}
	}
	method := u.User.Username()
	password, ok := u.User.Password()
	if !ok {
		bytes, err := decodeBase64String(method)
		if err != nil {
			return nil, sslocalUnknownSSError{origin}
		}
		slice := strings.SplitN(string(bytes), ":", 2)
		if len(slice) != 2 {
			return nil, sslocalUnknownSSError{origin}
		}
		method, password = slice[0], slice[1]
	}
	cipher, err := core.PickCipher(method, nil, password)
	if err != nil {
		return nil, sslocalUnknownCipherError{origin}
	}
	return sslocalDialer{u.Host, cipher, forward}, nil
}

type sslocalParseAddrError string

func (e sslocalParseAddrError) Error() string {
	return "invalid addr: " + string(e)
}

type sslocalUnknownSSError struct {
	u *url.URL
}

func (e sslocalUnknownSSError) Error() string {
	return "unknown ss: " + e.u.String()
}

type sslocalUnknownCipherError struct {
	u *url.URL
}

func (e sslocalUnknownCipherError) Error() string {
	return "unknown cipher: " + e.u.String()
}

func init() {
	proxy.RegisterDialerType("ss", sslocalFromURL)
}
