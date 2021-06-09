package chrome

import (
	"errors"

	"github.com/b97tsk/proxy"
	"github.com/b97tsk/proxy/loadbalance"

	// Import load balancing strategies.
	_ "github.com/b97tsk/proxy/loadbalance/strategy/failover"
	_ "github.com/b97tsk/proxy/loadbalance/strategy/random"

	"gopkg.in/yaml.v3"
)

type Proxy struct {
	x struct {
		Strategy string
		Dialers  []ProxyChain
	}
	d proxy.Dialer
}

func MakeProxy(urls ...string) (Proxy, error) {
	pc, err := MakeProxyChain(urls...)
	if err != nil {
		return Proxy{}, err
	}

	return ProxyFromProxyChain(pc), nil
}

func MakeProxyUsing(strategy string, dialers []ProxyChain) (Proxy, error) {
	if len(dialers) == 0 {
		return Proxy{}, errNoDialers
	}

	s := loadbalance.Get(strategy)
	if s == nil {
		return Proxy{}, errors.New("unknown strategy: " + strategy)
	}

	dialers1 := make([]proxy.Dialer, len(dialers))

	for i := range dialers1 {
		dialers1[i] = dialers[i].Dialer()
	}

	var p Proxy

	p.x.Strategy = strategy
	p.x.Dialers = dialers
	p.d = s(dialers1...)

	return p, nil
}

func ProxyFromProxyChain(pc ProxyChain) Proxy {
	var p Proxy

	if !pc.IsZero() {
		p.x.Dialers = []ProxyChain{pc}
		p.d = pc.Dialer()
	}

	return p
}

func (p Proxy) IsZero() bool {
	return len(p.x.Dialers) == 0
}

func (p Proxy) Equals(other Proxy) bool {
	return p.x.Strategy == other.x.Strategy &&
		isTwoProxyChainsIdentical(p.x.Dialers, other.x.Dialers)
}

func (p Proxy) Dialer() proxy.Dialer {
	if p.d != nil {
		return p.d
	}

	return proxy.Direct
}

func (p *Proxy) UnmarshalYAML(v *yaml.Node) error {
	var pc ProxyChain
	if err := pc.UnmarshalYAML(v); err == nil {
		*p = ProxyFromProxyChain(pc)
		return nil
	}

	var tmp Proxy
	if err := v.Decode(&tmp.x); err != nil {
		return errInvalidProxy
	}

	if tmp.x.Strategy == "" {
		return errStrategyNotSpecified
	}

	tmp, err := MakeProxyUsing(tmp.x.Strategy, tmp.x.Dialers)
	if err != nil {
		return err
	}

	*p = tmp

	return nil
}

func isTwoProxyChainsIdentical(a, b []ProxyChain) bool {
	if len(a) != len(b) {
		return false
	}

	for i, pc := range a {
		if !pc.Equals(b[i]) {
			return false
		}
	}

	return true
}

var (
	errNoDialers            = errors.New("no dialers")
	errInvalidProxy         = errors.New("invalid proxy")
	errStrategyNotSpecified = errors.New("strategy not specified")
)
