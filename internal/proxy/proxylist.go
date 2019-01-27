package proxy

import (
	"errors"
	"net/url"

	"golang.org/x/net/proxy"
)

type Proxy struct {
	URL *url.URL
	Raw string
}

type ProxyList []Proxy

func (pl ProxyList) Equals(other ProxyList) bool {
	if len(pl) != len(other) {
		return false
	}
	for i, p := range other {
		if p.Raw != pl[i].Raw {
			return false
		}
	}
	return true
}

func (pl ProxyList) NewDialer(forward proxy.Dialer) (proxy.Dialer, error) {
	var firstErr error
	for i := len(pl) - 1; i > -1; i-- {
		d, err := proxy.FromURL(pl[i].URL, forward)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		forward = d
	}
	return forward, firstErr
}

func (pl *ProxyList) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var rawurl string
	if err := unmarshal(&rawurl); err == nil {
		u, err := url.Parse(rawurl)
		if err != nil {
			return errors.New("invalid proxy: " + rawurl)
		}
		*pl = []Proxy{{u, rawurl}}
		_, err = pl.NewDialer(proxy.Direct)
		return err
	}
	var slice []string
	if err := unmarshal(&slice); err == nil {
		*pl = nil
		for _, rawurl := range slice {
			u, err := url.Parse(rawurl)
			if err != nil {
				return errors.New("invalid proxy: " + rawurl)
			}
			*pl = append(*pl, Proxy{u, rawurl})
		}
		_, err = pl.NewDialer(proxy.Direct)
		return err
	}
	return errors.New("invalid proxy list")
}
