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

type proxyChain struct {
	d proxy.Dialer
	s []proxyData
}

func makeProxyChain(urls ...string) (pc proxyChain, err error) {
	for _, s := range urls {
		if s == "" || strings.EqualFold(s, "Direct") {
			continue
		}

		u, err := url.Parse(s)
		if err != nil {
			return proxyChain{}, err
		}

		pc.s = append(pc.s, proxyData{u, s})
	}

	pc.d, err = pc.newDialer()
	if err != nil {
		return proxyChain{}, err
	}

	return pc, nil
}

func (pc proxyChain) IsZero() bool {
	switch pc.d {
	case nil, proxy.Direct:
		return true
	default:
		return false
	}
}

func (pc proxyChain) Equals(other proxyChain) bool {
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

func (pc proxyChain) Dialer() proxy.Dialer {
	if pc.d != nil {
		return pc.d
	}

	return proxy.Direct
}

func (pc proxyChain) newDialer() (proxy.Dialer, error) {
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

func (pc *proxyChain) UnmarshalYAML(v *yaml.Node) error {
	var urls []string
	if err := v.Decode(&urls); err != nil {
		var s string
		if err := v.Decode(&s); err != nil {
			return errInvalidProxyChain
		}

		urls = []string{s}
	}

	tmp, err := makeProxyChain(urls...)
	if err == nil {
		*pc = tmp
	}

	return err
}

var errInvalidProxyChain = errors.New("invalid proxy chain")
