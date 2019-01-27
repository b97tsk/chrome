package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/url"
	"strings"

	"github.com/b97tsk/chrome/internal/utility"
	"github.com/gogo/protobuf/proto"
	"gopkg.in/yaml.v2"
	"v2ray.com/core"
	_ "v2ray.com/core/main/distro/all"
	"v2ray.com/ext/tools/conf"
)

type vmessOptions struct {
	URL string
}

type vmessService struct{}

func (vmessService) Name() string {
	return "vmess"
}

func (vmessService) Run(ctx ServiceCtx) {
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
		options vmessOptions
	)
	for {
		select {
		case data := <-ctx.Events:
			if new, ok := data.(vmessOptions); ok {
				old := options
				options = new
				if new.URL != old.URL {
					if instance != nil {
						err := instance.Close()
						if err != nil {
							log.Printf("[vmess] close instance: %v\n", err)
						}
						instance = nil
						log.Printf("[vmess] stopped listening on %v\n", ctx.ListenAddr)
					}
					instance, err = vmessParseURL(new.URL, localAddr, localPort)
					if err != nil {
						log.Printf("[vmess] parse url: %v\n", err)
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

func (vmessService) UnmarshalOptions(text []byte) (interface{}, error) {
	var options vmessOptions
	if err := yaml.UnmarshalStrict(text, &options.URL); err != nil {
		return nil, err
	}
	return options, nil
}

func init() {
	services.Add("vmess", vmessService{})
}

func vmessParseURL(rawurl, localAddr, localPort string) (*core.Instance, error) {
	u, err := url.Parse(rawurl)
	if err != nil || u.Scheme != "vmess" {
		return nil, errors.New("invalid vmess: " + rawurl)
	}
	data, err := utility.DecodeBase64String(u.Host)
	if err != nil {
		return nil, errors.New("invalid vmess: " + rawurl)
	}

	var c struct {
		Version string `json:"v"`

		Net  string `json:"net"`
		TLS  string `json:"tls"`
		Type string `json:"type"`

		Address string `json:"add"`
		Port    string `json:"port"`
		ID      string `json:"id"`
		AlterID string `json:"aid"`

		Path string `json:"path"`
		Host string `json:"host"`

		LocalAddr string `json:"-"`
		LocalPort string `json:"-"`
	}

	if err := json.Unmarshal(data, &c); err != nil {
		return nil, errors.New("invalid vmess: " + rawurl)
	}

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
		return nil, errors.New("unknown vmess: " + rawurl)
	}

	c.LocalAddr, c.LocalPort = localAddr, localPort

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
