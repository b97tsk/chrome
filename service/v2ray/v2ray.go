package v2ray

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"net"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/b97tsk/chrome"
	"github.com/b97tsk/chrome/internal/ioutil"
	"github.com/b97tsk/chrome/internal/v2ray"
	"github.com/shadowsocks/go-shadowsocks2/socks"
)

type Options struct {
	ListenAddr string `yaml:"on"`
	ListenHost string `yaml:"-"`
	ListenPort string `yaml:"-"`

	Type      string
	Protocol  string `yaml:"-"`
	Transport string `yaml:"-"`

	ProtocolOptions  `yaml:",inline"`
	TransportOptions `yaml:",inline"`

	Policy struct {
		Handshake    int `json:"handshake,omitempty"`
		ConnIdle     int `json:"connIdle,omitempty"`
		UplinkOnly   int `json:"uplinkOnly"`
		DownlinkOnly int `json:"downlinkOnly"`
	}

	Proxy chrome.Proxy `yaml:"over"`

	ForwardServer struct {
		Address string
		Port    int
	} `yaml:"-"`

	Dial struct {
		Timeout time.Duration
	}
	Relay chrome.RelayOptions

	ins *v2ray.Instance
}

type ProtocolOptions struct {
	TROJAN struct {
		Clients []struct {
			Password string `json:"password"`
		}
	}
	VMESS struct {
		Clients []struct {
			ID      string `json:"id"`
			AlterID int    `json:"alterId,omitempty" yaml:"aid"`
		}
	}
}

type TransportOptions struct {
	HTTP struct {
		Host []string
		Path string
	}
	TCP struct{}
	TLS struct {
		Enabled      bool                 `json:"-" yaml:"-"`
		ServerName   string               `json:"serverName,omitempty"`
		Certificates []CertificateOptions `json:"certificates,omitempty" yaml:"-"`
		CertFile     chrome.EnvString     `json:"-"`
		KeyFile      chrome.EnvString     `json:"-"`
	}
	WS struct {
		Path   string
		Header map[string]string
	}
}

type CertificateOptions struct {
	Certificate []string `json:"certificate"`
	Key         []string `json:"key"`
}

type Service struct{}

const ServiceName = "v2ray"

func (Service) Name() string {
	return ServiceName
}

func (Service) Options() interface{} {
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

		ins1, err := createInstance(opts)
		if err != nil {
			logger.Errorf("create instance: %v", err)
			return
		}

		if err := ins1.Start(); err != nil {
			logger.Errorf("start instance: %v", err)
			return
		}

		ins = ins1
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
		case opts := <-ctx.Load:
			if new, ok := opts.(*Options); ok {
				old := <-optsOut
				new := *new
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
					certLines, err := readLines(ctx.Manager, new.TLS.CertFile.String())
					if err != nil {
						logger.Errorf("read cert file: %v", err)
						return
					}

					if len(certLines) == 0 {
						logger.Errorf("empty file: %v", new.TLS.CertFile)
						return
					}

					keyLines, err := readLines(ctx.Manager, new.TLS.KeyFile.String())
					if err != nil {
						logger.Errorf("read key file: %v", err)
						return
					}

					if len(keyLines) == 0 {
						logger.Errorf("empty file: %v", new.TLS.KeyFile)
						return
					}

					new.TLS.Certificates = []CertificateOptions{{certLines, keyLines}}
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
							opts, ok := <-optsOut
							if !ok {
								return
							}

							var reply bytes.Buffer

							rw := &struct {
								io.Reader
								io.Writer
							}{c, ioutil.LimitWriter(c, 2, &reply)}

							addr, err := socks.Handshake(rw)
							if err != nil {
								logger.Debugf("socks handshake: %v", err)
								return
							}

							local, localCtx := chrome.NewConnChecker(c)
							defer local.Close()

							remote, err := ctx.Manager.Dial(
								localCtx,
								opts.Proxy.Dialer(),
								"tcp",
								addr.String(),
								opts.Dial.Timeout,
							)
							if err != nil {
								logger.Trace(err)
								return
							}
							defer remote.Close()

							if _, err := reply.WriteTo(local); err != nil {
								logger.Trace(err)
								return
							}

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
			}
		case <-ctx.Loaded:
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

func createInstance(opts Options) (*v2ray.Instance, error) {
	opts.Protocol = "VMESS"
	opts.Transport = "TCP"

	for _, t := range strings.SplitN(opts.Type, "+", 3) {
		t = strings.ToUpper(t)
		switch t {
		case "TROJAN", "VMESS":
			opts.Protocol = t
		case "HTTP", "TCP", "WS":
			opts.Transport = t
		case "TLS":
			opts.TLS.Enabled = true
		case "":
		default:
			return nil, errors.New("unknown type: " + opts.Type)
		}
	}

	var buf bytes.Buffer
	if err := v2rayTemplate.Execute(&buf, &opts); err != nil {
		return nil, err
	}

	return v2ray.NewInstanceFromJSON(buf.Bytes())
}

func readLines(fsys fs.FS, name string) ([]string, error) {
	file, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string

	s := bufio.NewScanner(file)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}

	if err := s.Err(); err != nil {
		return nil, err
	}

	return lines, nil
}
