package vmess

import (
	"bytes"
	"errors"
	"log"
	"net"

	"github.com/b97tsk/chrome/internal/v2ray"
	"github.com/b97tsk/chrome/service"
	"gopkg.in/yaml.v2"
)

type Options struct {
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
	listenHost, listenPort, err := net.SplitHostPort(ctx.ListenAddr)
	if err != nil {
		log.Printf("[vmess] %v\n", err)
		return
	}

	var instance v2ray.Instance
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
					instance, err = createInstance(new, listenHost, listenPort)
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

func createInstance(options Options, listenHost, listenPort string) (v2ray.Instance, error) {
	c := struct {
		Options
		ListenHost string
		ListenPort string
	}{options, listenHost, listenPort}

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
		templateName = "ws"
	case "h2/tls", "ws/tls":
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

	return v2ray.NewInstanceFromJSON(buf.Bytes())
}
