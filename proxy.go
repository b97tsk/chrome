package chrome

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"math/rand"
	"net/url"
	"strings"

	"github.com/b97tsk/proxy"
	"github.com/b97tsk/proxy/loadbalance"

	// Import load balancing strategies.
	_ "github.com/b97tsk/proxy/loadbalance/strategy/failover"
	_ "github.com/b97tsk/proxy/loadbalance/strategy/random"
	_ "github.com/b97tsk/proxy/loadbalance/strategy/roundrobin"

	"gopkg.in/yaml.v3"
)

// Proxy is a helper type for unmarshaling a proxy from YAML.
// A Proxy is basically a proxy.Dialer.
type Proxy struct {
	d   proxy.Dialer
	sum []byte

	b struct {
		Strategy string
		Proxies  []Proxy
	}
}

// MakeProxy chains one or more proxies (parsed from urls) together, using
// fwd as a forwarder. The dial order would be: fwd -> proxies[0] ->
// proxies[1] -> ... -> proxies[n-1], where n is the number of proxies.
//
// Note that proxies specified in YAML use reverse ordering to go with
// the idiom "one over another".
func MakeProxy(fwd proxy.Dialer, urls ...string) (Proxy, error) {
	if fwd == nil {
		fwd = proxy.Direct
	}

	h := sha1.New()
	sep := []byte{'\n'}

	for _, s := range urls {
		if s == "" || strings.EqualFold(s, "Direct") {
			continue
		}

		u, err := url.Parse(s)
		if err != nil {
			return Proxy{}, err
		}

		d, err := proxy.FromURL(u, fwd)
		if err != nil {
			return Proxy{}, err
		}

		fwd = d

		_, _ = h.Write([]byte(s))
		_, _ = h.Write(sep)
	}

	var sum []byte
	if fwd != proxy.Direct {
		sum = h.Sum(make([]byte, 0, h.Size()))
	}

	return Proxy{d: fwd, sum: sum}, nil
}

// MakeProxyUsing creates a load balancing Proxy from multiple proxies with
// specified strategy.
func MakeProxyUsing(strategy string, proxies []Proxy) (Proxy, error) {
	return makeProxyUsing(strategy, proxies, false)
}

func makeProxyUsing(strategy string, proxies []Proxy, shuffle bool) (Proxy, error) {
	if len(proxies) == 0 {
		return Proxy{}, errNoProxies
	}

	s := loadbalance.Get(strategy)
	if s == nil {
		return Proxy{}, errors.New("unknown strategy: " + strategy)
	}

	dialers := make([]proxy.Dialer, len(proxies))

	for i := range dialers {
		dialers[i] = proxies[i].Dialer()
	}

	if shuffle {
		rand.Shuffle(len(dialers), func(i, j int) {
			dialers[i], dialers[j] = dialers[j], dialers[i]
		})
	}

	var p Proxy

	p.d = s(dialers)
	p.b.Strategy = strategy
	p.b.Proxies = proxies

	return p, nil
}

// IsZero reports whether p is proxy.Direct.
func (p Proxy) IsZero() bool {
	switch p.d {
	case nil, proxy.Direct:
		return true
	default:
		return false
	}
}

// Equal reports whether p and other are the same proxy.
func (p Proxy) Equal(other Proxy) bool {
	return bytes.Equal(p.sum, other.sum) &&
		p.b.Strategy == other.b.Strategy &&
		proxySliceEqual(p.b.Proxies, other.b.Proxies)
}

// Dialer gets the proxy.Dialer.
func (p Proxy) Dialer() proxy.Dialer {
	if p.d != nil {
		return p.d
	}

	return proxy.Direct
}

func (p *Proxy) UnmarshalYAML(v *yaml.Node) error {
	var urls []string
	if err := v.Decode(&urls); err != nil {
		var s string
		if err := v.Decode(&s); err != nil {
			return p.unmarshalYAML(v)
		}

		urls = []string{s}
	}

	for i, j := 0, len(urls)-1; i < j; i, j = i+1, j-1 {
		urls[i], urls[j] = urls[j], urls[i]
	}

	tmp, err := MakeProxy(nil, urls...)
	if err == nil {
		*p = tmp
	}

	return err
}

func (p *Proxy) unmarshalYAML(v *yaml.Node) error {
	var b struct {
		Strategy string
		Proxies  []Proxy
		Shuffle  bool
	}

	if err := v.Decode(&b); err != nil {
		return errInvalidProxy
	}

	if b.Strategy == "" {
		return errStrategyNotSpecified
	}

	tmp, err := makeProxyUsing(b.Strategy, b.Proxies, b.Shuffle)
	if err != nil {
		return err
	}

	*p = tmp

	return nil
}

func proxySliceEqual(a, b []Proxy) bool {
	if len(a) != len(b) {
		return false
	}

	for i, p := range a {
		if !p.Equal(b[i]) {
			return false
		}
	}

	return true
}

var (
	errNoProxies            = errors.New("no proxies")
	errInvalidProxy         = errors.New("invalid proxy")
	errStrategyNotSpecified = errors.New("strategy not specified")
)
