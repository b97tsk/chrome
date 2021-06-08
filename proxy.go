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

func (p Proxy) IsZero() bool {
	return p.x.Strategy == "" && len(p.x.Dialers) == 0
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
	var tmp Proxy

	var pc ProxyChain
	if err := pc.UnmarshalYAML(v); err == nil {
		if !pc.IsZero() {
			tmp.x.Dialers = []ProxyChain{pc}
		}

		p.x = tmp.x
		p.d = pc.Dialer()

		return nil
	}

	if err := v.Decode(&tmp.x); err != nil {
		return err
	}

	if tmp.x.Strategy == "" {
		return errors.New("strategy not specified")
	}

	s := loadbalance.Get(tmp.x.Strategy)
	if s == nil {
		return errors.New("unknown strategy: " + tmp.x.Strategy)
	}

	dialers := make([]proxy.Dialer, len(tmp.x.Dialers))

	for i := range dialers {
		dialers[i] = tmp.x.Dialers[i].Dialer()
	}

	p.x = tmp.x
	p.d = s(dialers...)

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
