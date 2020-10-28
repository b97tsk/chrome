package v2ray

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/golang/protobuf/proto"
	"v2ray.com/core"
	"v2ray.com/core/common/net"
	"v2ray.com/core/infra/conf"
	_ "v2ray.com/core/main/distro/all"
)

type Instance struct {
	inst *core.Instance
}

func NewInstanceFromJSON(data []byte) (*Instance, error) {
	var config conf.Config

	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	pb, err := config.Build()
	if err != nil {
		return nil, err
	}

	pbBytes, err := proto.Marshal(pb)
	if err != nil {
		return nil, err
	}

	pbBuffer := bytes.NewBuffer(pbBytes)
	coreConfig, err := core.LoadConfig("protobuf", ".pb", pbBuffer)
	if err != nil {
		return nil, err
	}

	inst, err := core.New(coreConfig)
	if err != nil {
		return nil, err
	}

	return &Instance{inst}, nil
}

func (v *Instance) Start() error {
	return v.inst.Start()
}

func (v *Instance) Close() error {
	return v.inst.Close()
}

func (v *Instance) Dial(network, addr string) (net.Conn, error) {
	return v.DialContext(context.Background(), network, addr)
}

func (v *Instance) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return v.dialTCP(ctx, network, addr)
	}
	return nil, errors.New("unsupported network " + network)
}

func (v *Instance) dialTCP(ctx context.Context, _, addr string) (net.Conn, error) {
	dest, err := net.ParseDestination("tcp:" + addr)
	if err != nil {
		return nil, err
	}
	return core.Dial(ctx, v.inst, dest)
}
