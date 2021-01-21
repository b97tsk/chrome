package service

import (
	"errors"
	"net/url"
	"strings"

	"github.com/b97tsk/chrome/internal/proxy"
	"gopkg.in/yaml.v3"
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

func (pc ProxyChain) NewDialer() proxy.Dialer {
	d, _ := pc.newDialer()
	return d
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
	var rawurl string
	if err := v.Decode(&rawurl); err == nil {
		if strings.EqualFold(rawurl, "DIRECT") {
			pc.s = nil
			return nil
		}

		u, err := url.Parse(rawurl)
		if err != nil {
			return errors.New("invalid proxy: " + rawurl)
		}

		pc.s = []proxyData{{u, rawurl}}
		_, err = pc.newDialer()

		return err
	}

	var slice []string
	if err := v.Decode(&slice); err == nil {
		pc.s = nil

		for _, rawurl := range slice {
			if strings.EqualFold(rawurl, "DIRECT") {
				continue
			}

			u, err := url.Parse(rawurl)
			if err != nil {
				return errors.New("invalid proxy: " + rawurl)
			}

			pc.s = append(pc.s, proxyData{u, rawurl})
		}

		_, err = pc.newDialer()

		return err
	}

	return errors.New("invalid proxy chain")
}
