package v2ray

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	core "github.com/v2fly/v2ray-core/v4"
	"github.com/v2fly/v2ray-core/v4/common/net"
	"github.com/v2fly/v2ray-core/v4/infra/conf"
	_ "github.com/v2fly/v2ray-core/v4/main/distro/all" // Imported for side-effect.
	"google.golang.org/protobuf/proto"
)

type instance struct {
	ins *core.Instance
}

func newInstanceFromJSON(data []byte) (*instance, error) {
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

	ins, err := core.New(coreConfig)
	if err != nil {
		return nil, err
	}

	return &instance{ins}, nil
}

func (v *instance) Start() error {
	return v.ins.Start()
}

func (v *instance) Close() error {
	return v.ins.Close()
}

func (v *instance) Dial(network, addr string) (net.Conn, error) {
	return v.DialContext(context.Background(), network, addr)
}

func (v *instance) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
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
