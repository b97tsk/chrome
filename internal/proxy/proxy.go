package proxy

import (
	"net/url"

	"golang.org/x/net/proxy"
)

var Direct = proxy.Direct

type (
	Dialer        = proxy.Dialer
	ContextDialer = proxy.ContextDialer
)

func FromURL(u *url.URL, forward proxy.Dialer) (proxy.Dialer, error) {
	return proxy.FromURL(u, forward)
}
