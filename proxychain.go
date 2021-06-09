package chrome

import (
	"errors"
	"net/url"
	"strings"

	"github.com/b97tsk/proxy"
	"gopkg.in/yaml.v3"
)

type proxyData struct {
	URL *url.URL
	Raw string
}

type ProxyChain struct {
	s []proxyData
	d proxy.Dialer
}

func MakeProxyChain(urls ...string) (pc ProxyChain, err error) {
	for _, s := range urls {
		if s == "" || strings.EqualFold(s, "Direct") {
			continue
		}

		u, err := url.Parse(s)
		if err != nil {
			return ProxyChain{}, err
		}

		pc.s = append(pc.s, proxyData{u, s})
	}

	pc.d, err = pc.newDialer()
	if err != nil {
		return ProxyChain{}, err
	}

	return pc, nil
}

func (pc ProxyChain) IsZero() bool {
	return len(pc.s) == 0
}

func (pc ProxyChain) Equals(other ProxyChain) bool {
	if len(pc.s) != len(other.s) {
		return false
	}

	for i, p := range other.s {
		if p.Raw != pc.s[i].Raw {
			return false
		}
	}

	return true
}

func (pc ProxyChain) Dialer() proxy.Dialer {
	if pc.d != nil {
		return pc.d
	}

	return proxy.Direct
}

func (pc ProxyChain) newDialer() (proxy.Dialer, error) {
	var forward proxy.Dialer = proxy.Direct

	for i := len(pc.s) - 1; i > -1; i-- {
		d, err := proxy.FromURL(pc.s[i].URL, forward)
		if err != nil {
			return nil, err
		}

		forward = d
	}

	return forward, nil
}

func (pc *ProxyChain) UnmarshalYAML(v *yaml.Node) error {
	var urls []string
	if err := v.Decode(&urls); err != nil {
		var s string
		if err := v.Decode(&s); err != nil {
			return errInvalidProxyChain
		}

		urls = []string{s}
	}

	tmp, err := MakeProxyChain(urls...)
	if err == nil {
		*pc = tmp
	}

	return err
}

var errInvalidProxyChain = errors.New("invalid proxy chain")
