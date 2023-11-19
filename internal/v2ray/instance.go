package v2ray

import (
	"context"
	"errors"

	core "github.com/v2fly/v2ray-core/v5"
	"github.com/v2fly/v2ray-core/v5/common/net"
	_ "github.com/v2fly/v2ray-core/v5/main/distro/all" // Imported for side-effect.
)

type Instance struct {
	ins *core.Instance
}

func StartInstance(data []byte) (*Instance, error) {
	ins, err := core.StartInstance("jsonv5", data)
	if err != nil {
		return nil, err
	}

	return &Instance{ins}, nil
}

func (v *Instance) Close() error {
	return v.ins.Close()
}

func (v *Instance) Dial(network, addr string) (net.Conn, error) {
	return v.DialContext(context.Background(), network, addr)
}

func (v *Instance) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, errors.New("network not implemented: " + network)
	}

	dest, err := net.ParseDestination("tcp:" + addr)
	if err != nil {
		return nil, err
	}

	return core.Dial(ctx, v.ins, dest)
}
