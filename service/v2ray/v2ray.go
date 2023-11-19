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

	Dial  chrome.DialOptions
	Relay chrome.RelayOptions

	ins *v2ray.Instance
}

type ProtocolOptions struct {
	TROJAN, VLESS, VMESS struct {
		Users []string `json:"users"`
	}
}

type TransportOptions struct {
	GRPC struct {
		ServiceName string `json:"serviceName"`
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
	logger := ctx.Manager.Logger(ServiceName)

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

						go ctx.Manager.Serve(ln, func(c net.Conn) {
							var reply bytes.Buffer

							rw := &struct {
								io.Reader
								io.Writer
							}{c, ioutil.LimitWriter(c, 2, &reply)}

							addr, err := socks.Handshake(rw)
							if err != nil {
								return
							}

							remoteAddr := addr.String()

							getopts := func() (chrome.RelayOptions, bool) {
								opts, ok := <-optsOut
								return opts.Relay, ok
							}

							getRemote := func(localCtx context.Context) net.Conn {
								getopts := func() (chrome.Proxy, chrome.DialOptions, bool) {
									opts, ok := <-optsOut
									return opts.Proxy, opts.Dial, ok
								}

								remote, _ := ctx.Manager.Dial(localCtx, "tcp", remoteAddr, getopts, logger)

								return remote
							}

							sendResponse := func(w io.Writer) bool {
								if _, err := reply.WriteTo(w); err != nil {
									logger.Tracef("write response to local: %v", err)
									return false
								}

								return true
							}

							ctx.Manager.Relay(c, getopts, getRemote, sendResponse, logger)
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
	x.Dial, y.Dial = z.Dial, z.Dial
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
		case "TROJAN", "VLESS", "VMESS":
			opts.Protocol = t
		case "GRPC", "TCP", "WS":
			opts.Transport = t
		case "TLS":
			opts.Security = t
		case "":
		default:
			return nil, fmt.Errorf("unknown type: %v", opts.Type)
		}
	}

	var buf bytes.Buffer
	if err := v2rayTemplate.Execute(&buf, &opts); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
