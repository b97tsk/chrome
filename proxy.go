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
}

func (opts ProxyOptions) IsZero() bool {
	return opts.x.Strategy == "" && len(opts.x.Dialers) == 0
}

func (opts ProxyOptions) Equals(other ProxyOptions) bool {
	return opts.x.Strategy == other.x.Strategy &&
		isTwoProxyChainsIdentical(opts.x.Dialers, other.x.Dialers)
}

func (opts ProxyOptions) NewDialer() proxy.Dialer {
	if len(opts.x.Dialers) == 0 {
		return proxy.Direct
	}

	if opts.x.Strategy == "" {
		return opts.x.Dialers[0].NewDialer()
	}

	dialers := make([]proxy.Dialer, len(opts.x.Dialers))

	for i := range dialers {
		dialers[i] = opts.x.Dialers[i].NewDialer()
	}

	return loadbalance.Get(opts.x.Strategy)(dialers...)
}

func (opts *ProxyOptions) UnmarshalYAML(v *yaml.Node) error {
	var pc ProxyChain
	if err := pc.UnmarshalYAML(v); err == nil {
		opts.x.Strategy = ""
		opts.x.Dialers = nil

		if !pc.IsZero() {
			opts.x.Dialers = []ProxyChain{pc}
		}

		return nil
	}

	var tmp ProxyOptions
	if err := v.Decode(&tmp.x); err != nil {
		return err
	}

	if s := loadbalance.Get(tmp.x.Strategy); s == nil {
		return fmt.Errorf("unknown loadbalance strategy: %v", tmp.x.Strategy)
	}

	opts.x = tmp.x

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
