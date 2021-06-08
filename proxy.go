package chrome

import (
	"fmt"

	"github.com/b97tsk/proxy"
	"github.com/b97tsk/proxy/loadbalance"

	// Import load balancing strategies.
	_ "github.com/b97tsk/proxy/loadbalance/strategy/failover"
	_ "github.com/b97tsk/proxy/loadbalance/strategy/random"

	"gopkg.in/yaml.v3"
)

type ProxyOptions struct {
	x struct {
		Strategy string
		Dialers  []ProxyChain
	}
	d proxy.Dialer
}

func (opts ProxyOptions) IsZero() bool {
	return opts.x.Strategy == "" && len(opts.x.Dialers) == 0
}

func (opts ProxyOptions) Equals(other ProxyOptions) bool {
	return opts.x.Strategy == other.x.Strategy &&
		isTwoProxyChainsIdentical(opts.x.Dialers, other.x.Dialers)
}

func (opts ProxyOptions) Dialer() proxy.Dialer {
	if opts.d != nil {
		return opts.d
	}

	return proxy.Direct
}

func (opts *ProxyOptions) UnmarshalYAML(v *yaml.Node) error {
	var tmp ProxyOptions

	var pc ProxyChain
	if err := pc.UnmarshalYAML(v); err == nil {
		if !pc.IsZero() {
			tmp.x.Dialers = []ProxyChain{pc}
		}

		opts.x = tmp.x
		opts.d = pc.Dialer()

		return nil
	}

	if err := v.Decode(&tmp.x); err != nil {
		return err
	}

	s := loadbalance.Get(tmp.x.Strategy)
	if s == nil {
		return fmt.Errorf("unknown loadbalance strategy: %v", tmp.x.Strategy)
	}

	dialers := make([]proxy.Dialer, len(tmp.x.Dialers))

	for i := range dialers {
		dialers[i] = tmp.x.Dialers[i].Dialer()
	}

	opts.x = tmp.x
	opts.d = s(dialers...)

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
