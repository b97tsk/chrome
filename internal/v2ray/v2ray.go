package v2ray

import (
	"bytes"
	"encoding/json"

	"github.com/gogo/protobuf/proto"
	"v2ray.com/core"
	_ "v2ray.com/core/main/distro/all"
	"v2ray.com/ext/tools/conf"
)

type Instance interface {
	Start() error
	Close() error
}

func NewInstanceFromJSON(data []byte) (Instance, error) {
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

	return core.New(coreConfig)
}
