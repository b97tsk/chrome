package v2ray

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"reflect"
	"strconv"
	"strings"

	"github.com/b97tsk/chrome"
	"github.com/b97tsk/chrome/internal/ioutil"
	"github.com/b97tsk/chrome/internal/v2ray"
	"github.com/b97tsk/proxy"
	"github.com/shadowsocks/go-shadowsocks2/socks"
)

type Options struct {
	ListenAddr string `yaml:"on"`
	ListenHost string `yaml:"-"`
	ListenPort string `yaml:"-"`

	Proxy chrome.Proxy `yaml:"over"`

	ForwardServer struct {
		Address string `json:"address"`
		Port    int    `json:"port"`
	} `yaml:"-"`

	Type      string
	Protocol  string `yaml:"-"`
	Transport string `yaml:"-"`
	Security  string `yaml:"-"`

	ProtocolOptions  `yaml:",inline"`
	TransportOptions `yaml:",inline"`
	SecurityOptions  `yaml:",inline"`

	Conn  chrome.ConnOptions
	Relay chrome.RelayOptions

	ins *v2ray.Instance
}

type ProtocolOptions struct {
	SHADOWSOCKS struct {
		Method   string `json:"method"`
		Password string `json:"password"`
	}
	TROJAN, VLESS, VMESS struct {
		Users []string `json:"users"`
	}
}

type TransportOptions struct {
	GRPC struct {
		ServiceName string `json:"serviceName"`
	}
	HTTPUPGRADE struct {
		Path string `json:"path,omitempty"`
		Host string `json:"host,omitempty"`
	}
	TCP struct{}
	WS  struct {
		Path   string `json:"path,omitempty"`
		Header []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"header,omitempty"`
	}
}

type SecurityOptions struct {
	TLS struct {
		ServerName  string               `json:"serverName,omitempty"`
		Certificate []CertificateOptions `json:"certificate,omitempty" yaml:"-"`
		CertFile    chrome.EnvString     `json:"-"`
		KeyFile     chrome.EnvString     `json:"-"`
	}
}

type CertificateOptions struct {
	Certificate string `json:"certificate"`
	Key         string `json:"key"`
}

type Service struct{}

const ServiceName = "v2ray"

func (Service) Name() string {
	return ServiceName
}

func (Service) Options() any {
	return new(Options)
}

func (Service) Run(ctx chrome.Context) {
	logger := ctx.Manager.Logger(ctx.JobName)

	optsIn, optsOut := make(chan Options), make(chan Options)
	defer close(optsIn)

	go func() {
		var opts Options

		ok := true
		for ok {
			select {
			case opts, ok = <-optsIn:
			case optsOut <- opts:
			}
		}

		close(optsOut)
	}()

	var (
		ins   *v2ray.Instance
		laddr net.Addr
	)

	startInstance := func(opts Options) {
		ln, err := net.Listen("tcp", opts.ListenAddr)
		if err != nil {
			logger.Error(err)
			return
		}

		ln.Close()

		data, err := parseOptions(opts)
		if err != nil {
			logger.Errorf("parse options: %v", err)
			return
		}

		ins, err = v2ray.StartInstance(data)
		if err != nil {
			logger.Errorf("start instance: %v", err)
			return
		}

		laddr = ln.Addr()

		logger.Infof("listening on %v", laddr)
	}

	stopInstance := func() {
		if ins != nil {
			if err := ins.Close(); err != nil {
				logger.Debugf("close instance: %v", err)
			}

			ins = nil

			logger.Infof("stopped listening on %v", laddr)
		}
	}
	defer stopInstance()

	var forwardListener net.Listener
	defer func() {
		if forwardListener != nil {
			_ = forwardListener.Close()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ctx.Event:
			switch ev := ev.(type) {
			case chrome.LoadEvent:
				old := <-optsOut
				new := *ev.Options.(*Options)
				new.ins = old.ins

				{
					host, port, err := net.SplitHostPort(new.ListenAddr)
					if err != nil {
						logger.Error(err)
						return
					}

					new.ListenHost, new.ListenPort = host, port
				}

				if new.ListenAddr != old.ListenAddr {
					stopInstance()

					new.ins = nil
				}

				if new.TLS.CertFile != "" && new.TLS.KeyFile != "" {
					certData, err := fs.ReadFile(ctx.Manager, new.TLS.CertFile.String())
					if err != nil {
						logger.Errorf("read cert file: %v", err)
						return
					}

					if len(certData) == 0 {
						logger.Errorf("empty file: %v", new.TLS.CertFile)
						return
					}

					keyData, err := fs.ReadFile(ctx.Manager, new.TLS.KeyFile.String())
					if err != nil {
						logger.Errorf("read key file: %v", err)
						return
					}

					if len(keyData) == 0 {
						logger.Errorf("empty file: %v", new.TLS.KeyFile)
						return
					}

					new.TLS.Certificate = []CertificateOptions{{string(certData), string(keyData)}}
				}

				if !new.Proxy.IsZero() {
					if forwardListener == nil {
						ln, err := net.Listen("tcp", "localhost:")
						if err != nil {
							logger.Errorf("start forward server: %v", err)
							return
						}

						forwardListener = ln

						go ctx.Manager.Serve(ln, func(local net.Conn) {
							var reply bytes.Buffer

							rw := &struct {
								io.Reader
								io.Writer
							}{local, ioutil.LimitWriter(local, 2, &reply)}

							addr, err := socks.Handshake(rw)
							if err != nil {
								return
							}

							opts, ok := <-optsOut
							if !ok {
								return
							}

							if _, err := reply.WriteTo(local); err != nil {
								logger.Tracef("write response to local: %v", err)
								return
							}

							remoteAddr := addr.String()

							getRemote := func(ctx context.Context) (net.Conn, error) {
								opts, ok := <-optsOut
								if !ok {
									return nil, chrome.CloseConn
								}

								return proxy.Dial(ctx, opts.Proxy.Dialer(), "tcp", remoteAddr)
							}

							remote := ctx.Manager.NewConn(remoteAddr, getRemote, opts.Conn, opts.Relay, logger, nil)
							defer remote.Close()

							ctx.Manager.Relay(local, remote, opts.Relay)
						})
					}

					host, port, _ := net.SplitHostPort(forwardListener.Addr().String())
					new.ForwardServer.Address = host
					new.ForwardServer.Port, _ = strconv.Atoi(port)
				}

				if shouldRestart(old, new) {
					stopInstance()

					new.ins = nil
				}

				optsIn <- new
			case chrome.LoadedEvent:
				if ins == nil {
					opts := <-optsOut
					startInstance(opts)

					if ins == nil {
						return
					}

					opts.ins = ins
					optsIn <- opts
				}
			}
		}
	}
}

func shouldRestart(x, y Options) bool {
	if x.Proxy.IsZero() != y.Proxy.IsZero() {
		return true
	}

	var z Options
	x.Proxy, y.Proxy = z.Proxy, z.Proxy
	x.Conn, y.Conn = z.Conn, z.Conn
	x.Relay, y.Relay = z.Relay, z.Relay
	x.ins, y.ins = z.ins, z.ins

	return !reflect.DeepEqual(x, y)
}

func parseOptions(opts Options) ([]byte, error) {
	opts.Protocol = "VMESS"
	opts.Transport = "TCP"

	for _, t := range strings.SplitN(opts.Type, "+", 3) {
		t = strings.ToUpper(t)
		switch t {
		case "SHADOWSOCKS", "TROJAN", "VLESS", "VMESS":
			opts.Protocol = t
		case "GRPC", "HTTPUPGRADE", "TCP", "WS":
			opts.Transport = t
		case "TLS":
			opts.Security = t
		case "":
		default:
			return nil, fmt.Errorf("unknown type: %v", opts.Type)
		}
	}

	if opts.Protocol == "SHADOWSOCKS" {
		switch normalizeMethod(opts.SHADOWSOCKS.Method) {
		case "aes-128-gcm":
			opts.SHADOWSOCKS.Method = "AES_128_GCM"
		case "aes-256-gcm":
			opts.SHADOWSOCKS.Method = "AES_256_GCM"
		case "chacha20-poly1305", "chacha20-ietf-poly1305":
			opts.SHADOWSOCKS.Method = "CHACHA20_POLY1305"
		default:
			return nil, fmt.Errorf("unknown method: %v", orEmpty(opts.SHADOWSOCKS.Method))
		}
	}

	var buf bytes.Buffer
	if err := v2rayTemplate.Execute(&buf, &opts); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func normalizeMethod(s string) string {
	return strings.ReplaceAll(strings.ToLower(s), "_", "-")
}

func orEmpty(s string) string {
	if s == "" {
		return "(empty)"
	}
	return s
}
