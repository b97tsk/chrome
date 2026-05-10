// Package proxy is a library based on golang.org/x/net/proxy.
package proxy

import (
	"net"
	"net/url"
)

// A Dialer is a means to establish a connection.
// Custom dialers should also implement ContextDialer.
type Dialer interface {
	Dial(network, addr string) (net.Conn, error)
}

// Direct implements Dialer by making network connections directly using [net.Dial] or [net.DialContext].
var Direct Dialer = proxy_Direct

// FromEnvironment returns the dialer specified by the proxy-related
// variables in the environment and makes underlying connections
// directly.
func FromEnvironment() Dialer {
	return proxy_FromEnvironmentUsing(Direct)
}

// FromEnvironmentUsing returns the dialer specify by the proxy-related
// variables in the environment and makes underlying connections
// using the provided forwarding Dialer (for instance, a *net.Dialer
// with desired configuration).
func FromEnvironmentUsing(forward Dialer) Dialer {
	return proxy_FromEnvironmentUsing(forward)
}

// RegisterDialerType takes a URL scheme and a function to generate Dialers from
// a URL with that scheme and a forwarding Dialer. Registered schemes are used
// by FromURL.
func RegisterDialerType(scheme string, f func(*url.URL, Dialer) (Dialer, error)) {
	proxy_RegisterDialerType(scheme, func(u *url.URL, fwd proxy_Dialer) (proxy_Dialer, error) {
		return f(u, fwd)
	})
}

// FromURL returns a Dialer given a URL specification and an underlying
// Dialer for it to make network requests.
func FromURL(u *url.URL, forward Dialer) (Dialer, error) {
	switch u.Scheme {
	case "socks5", "socks5h":
		u2 := *u
		u = &u2
		u.Scheme = "socks"
	}
	return proxy_FromURL(u, forward)
}
