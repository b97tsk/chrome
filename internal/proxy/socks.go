package proxy

import (
	"net/url"

	"golang.org/x/net/proxy"
)

func init() {
	proxy.RegisterDialerType("socks", socksFromURL)
	proxy.RegisterDialerType("socks5", socksFromURL)
}

func socksFromURL(u *url.URL, forward proxy.Dialer) (proxy.Dialer, error) {
	var auth *proxy.Auth
	if u.User != nil {
		auth = new(proxy.Auth)
		auth.User = u.User.Username()

		if p, ok := u.User.Password(); ok {
			auth.Password = p
		}
	}

	return proxy.SOCKS5("tcp", u.Host, auth, forward)
}
