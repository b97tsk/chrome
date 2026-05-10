package chrome

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"net"
	"net/url"
	"strings"

	"github.com/b97tsk/proxy"
	"gopkg.in/yaml.v3"
)

// Proxy is a helper type for unmarshaling a proxy from YAML.
// A Proxy is basically a proxy.Dialer.
type Proxy struct {
	d   proxy.Dialer
	sum []byte
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
		switch {
		case s == "" || strings.EqualFold(s, "Direct"):
			continue
		case strings.EqualFold(s, "Block"):
			return Proxy{d: blockOrReset(true), sum: []byte("Block")}, nil
		case strings.EqualFold(s, "Reset"):
			return Proxy{d: blockOrReset(false), sum: []byte("Reset")}, nil
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

type blockOrReset bool

func (blockOrReset) Dial(network, addr string) (net.Conn, error) {
	panic("not implemented")
}

func (block blockOrReset) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	return nil, CloseConn
}

// ProxyFromDialer creates a Proxy from a Dialer.
// The Dialer returned shouldn't be used for comparison.
func ProxyFromDialer(d proxy.Dialer) Proxy { return Proxy{d: d} }

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
	return bytes.Equal(p.sum, other.sum)
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
			return errInvalidProxy
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

var errInvalidProxy = errors.New("invalid proxy")
