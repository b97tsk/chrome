package chrome

import (
	"errors"
	"math/rand"

	"github.com/b97tsk/proxy"
	"github.com/b97tsk/proxy/loadbalance"

	// Import load balancing strategies.
	_ "github.com/b97tsk/proxy/loadbalance/strategy/failover"
	_ "github.com/b97tsk/proxy/loadbalance/strategy/random"
	_ "github.com/b97tsk/proxy/loadbalance/strategy/roundrobin"

	"gopkg.in/yaml.v3"
)

type Proxy struct {
	pc proxyChain
	b  struct {
		Strategy string
		Dialers  []Proxy
	}
}

func MakeProxy(urls ...string) (Proxy, error) {
	pc, err := makeProxyChain(urls...)
	if err != nil {
		return Proxy{}, err
	}

	return Proxy{pc: pc}, nil
}

func MakeProxyUsing(strategy string, dialers []Proxy) (Proxy, error) {
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

	p.pc.d = s(dialers1)
	p.b.Strategy = strategy
	p.b.Dialers = dialers

	return p, nil
}

func (p Proxy) IsZero() bool {
	return p.pc.IsZero()
}

func (p Proxy) Equals(other Proxy) bool {
	return p.pc.Equals(other.pc) &&
		p.b.Strategy == other.b.Strategy &&
		proxySliceEqual(p.b.Dialers, other.b.Dialers)
}

func (p Proxy) Dialer() proxy.Dialer {
	return p.pc.Dialer()
}

func (p *Proxy) UnmarshalYAML(v *yaml.Node) error {
	var pc proxyChain
	if err := pc.UnmarshalYAML(v); err == nil {
		*p = Proxy{pc: pc}
		return nil
	}

	var b struct {
		Strategy string
		Dialers  []Proxy
		Shuffle  bool
	}

	if err := v.Decode(&b); err != nil {
		return errInvalidProxy
	}

	if b.Strategy == "" {
		return errStrategyNotSpecified
	}

	if b.Shuffle {
		rand.Shuffle(len(b.Dialers), func(i, j int) {
			b.Dialers[i], b.Dialers[j] = b.Dialers[j], b.Dialers[i]
		})
	}

	tmp, err := MakeProxyUsing(b.Strategy, b.Dialers)
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
