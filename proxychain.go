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
	var tmp ProxyChain

	var s string
	if err := v.Decode(&s); err == nil {
		if strings.EqualFold(s, "DIRECT") {
			*pc = tmp
			return nil
		}

		u, err := url.Parse(s)
		if err != nil {
			return errors.New("invalid proxy: " + s)
		}

		tmp.s = []proxyData{{u, s}}
		tmp.d, err = tmp.newDialer()

		if err == nil {
			*pc = tmp
		}

		return err
	}

	var slice []string
	if err := v.Decode(&slice); err == nil {
		for _, s := range slice {
			if strings.EqualFold(s, "DIRECT") {
				continue
			}

			u, err := url.Parse(s)
			if err != nil {
				return errors.New("invalid proxy: " + s)
			}

			tmp.s = append(tmp.s, proxyData{u, s})
		}

		tmp.d, err = tmp.newDialer()

		if err == nil {
			*pc = tmp
		}

		return err
	}

	return errors.New("invalid proxy chain")
}
