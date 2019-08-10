package service

import (
	"errors"
	"net/url"

	"github.com/b97tsk/chrome/internal/proxy"
)

type proxyData struct {
	URL *url.URL
	Raw string
}

type ProxyChain struct {
	s []proxyData
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

func (pc ProxyChain) NewDialer(forward proxy.Dialer) (proxy.Dialer, error) {
	var firstErr error
	for i := len(pc.s) - 1; i > -1; i-- {
		d, err := proxy.FromURL(pc.s[i].URL, forward)
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

func (pc *ProxyChain) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var rawurl string
	if err := unmarshal(&rawurl); err == nil {
		u, err := url.Parse(rawurl)
		if err != nil {
			return errors.New("invalid proxy: " + rawurl)
		}
		pc.s = []proxyData{{u, rawurl}}
		_, err = pc.NewDialer(proxy.Direct)
		return err
	}
	var slice []string
	if err := unmarshal(&slice); err == nil {
		pc.s = nil
		for _, rawurl := range slice {
			u, err := url.Parse(rawurl)
			if err != nil {
				return errors.New("invalid proxy: " + rawurl)
			}
			pc.s = append(pc.s, proxyData{u, rawurl})
		}
		_, err = pc.NewDialer(proxy.Direct)
		return err
	}
	return errors.New("invalid proxy chain")
}
