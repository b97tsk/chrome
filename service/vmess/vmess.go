package vmess

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net"
	"strings"

	"github.com/b97tsk/chrome/service"
	"github.com/gogo/protobuf/proto"
	"gopkg.in/yaml.v2"
	"v2ray.com/core"
	_ "v2ray.com/core/main/distro/all"
	"v2ray.com/ext/tools/conf"
)

type Options struct {
	Version    string `yaml:"v"`
	Postscript string `yaml:"ps"`

	Address string `yaml:"add"`
	Port    string `yaml:"port"`
	ID      string `yaml:"id"`
	AlterID string `yaml:"aid"`

	Net  string `yaml:"net"`
	TLS  string `yaml:"tls"`
	Type string `yaml:"type"`

	Path string `yaml:"path"`
	Host string `yaml:"host"`
}

type Service struct{}

func (Service) Name() string {
	return "vmess"
}

func (Service) Run(ctx service.Context) {
	localAddr, localPort, err := net.SplitHostPort(ctx.ListenAddr)
	if err != nil {
		log.Printf("[vmess] %v\n", err)
		return
	}

	var instance *core.Instance
	defer func() {
		if instance != nil {
			err := instance.Close()
			if err != nil {
				log.Printf("[vmess] close instance: %v\n", err)
			}
			instance = nil
			log.Printf("[vmess] stopped listening on %v\n", ctx.ListenAddr)
		}
	}()

	var (
		options Options
	)
	for {
		select {
		case data := <-ctx.Events:
			if new, ok := data.(Options); ok {
				old := options
				options = new
				if new != old {
					if instance != nil {
						err := instance.Close()
						if err != nil {
							log.Printf("[vmess] close instance: %v\n", err)
						}
						instance = nil
						log.Printf("[vmess] stopped listening on %v\n", ctx.ListenAddr)
					}
					instance, err = createInstance(new, localAddr, localPort)
					if err != nil {
						log.Printf("[vmess] create instance: %v\n", err)
						break
					}
					err = instance.Start()
					if err != nil {
						log.Printf("[vmess] start instance: %v\n", err)
						break
					}
					log.Printf("[vmess] listening on %v\n", ctx.ListenAddr)
				}
			}
		case <-ctx.Done:
			return
		}
	}
}

func (Service) UnmarshalOptions(text []byte) (interface{}, error) {
	var options Options
	if err := yaml.UnmarshalStrict(text, &options); err != nil {
		return nil, err
	}
	return options, nil
}

func createInstance(options Options, localAddr, localPort string) (*core.Instance, error) {
	c := struct {
		Options
		LocalAddr string
		LocalPort string
	}{options, localAddr, localPort}

	var templateName string

	switch c.Net + "/" + c.TLS {
	case "kcp/", "kcp/none":
		if c.Type == "" {
			c.Type = "none"
		}
		templateName = "kcp"
	case "tcp/", "tcp/none":
		templateName = "tcp"
		if c.Type == "http" {
			if c.Version == "" {
				fields := strings.SplitN(c.Path, ";", 2)
				if len(fields) == 2 {
					c.Path, c.Host = fields[0], fields[1]
				}
			}
			if c.Path == "" {
				c.Path = "/"
			}
			if c.Host == "" {
				c.Host = c.Address
			}
			templateName = "tcp/http"
		}
	case "tcp/tls":
		if c.Host == "" {
			c.Host = c.Address
		}
		templateName = "tcp/tls"
	case "ws/", "ws/none":
		if c.Version == "" && c.Path == "" {
			c.Path = c.Host
		}
		templateName = "ws"
	case "h2/tls", "ws/tls":
		if c.Version == "" {
			fields := strings.SplitN(c.Path, ";", 2)
			if len(fields) == 2 {
				c.Path, c.Host = fields[0], fields[1]
			}
		}
		if c.Path == "" {
			c.Path = "/"
		}
		if c.Host == "" {
			c.Host = c.Address
		}
		templateName = c.Net + "/" + c.TLS
	}

	tpl := vmessTemplate.Lookup(templateName)
	if tpl == nil {
		return nil, errors.New("unknown vmess type: " + templateName)
	}

	buf := new(bytes.Buffer)

	if err := tpl.Execute(buf, &c); err != nil {
		return nil, err
	}

	config := conf.Config{}

	if err := json.Unmarshal(buf.Bytes(), &config); err != nil {
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
