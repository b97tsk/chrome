package v2ray

import (
	"context"
	"encoding/json"
	"errors"

	core "github.com/v2fly/v2ray-core/v5"
	"github.com/v2fly/v2ray-core/v5/common/net"
	conf "github.com/v2fly/v2ray-core/v5/infra/conf/v4"
	_ "github.com/v2fly/v2ray-core/v5/main/distro/all" // Imported for side-effect.
)

type Instance struct {
	ins *core.Instance
}

func NewInstanceFromJSON(data []byte) (*Instance, error) {
	var config conf.Config

	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	coreConfig, err := config.Build()
	if err != nil {
		return nil, err
	}

	ins, err := core.New(coreConfig)
	if err != nil {
		return nil, err
	}

	return &Instance{ins}, nil
}

func (v *Instance) Start() error {
	return v.ins.Start()
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
