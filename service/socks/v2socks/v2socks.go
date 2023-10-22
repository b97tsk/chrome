package v2socks

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
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

	Proxy chrome.Proxy `yaml:"over"`

	ForwardServer HostportOptions `yaml:"-"`

	URL string

	Type      string
	Protocol  string `yaml:"-"`
	Transport string `yaml:"-"`

	ProtocolOptions  `yaml:",inline"`
	TransportOptions `yaml:",inline"`

	Mux struct {
		Enabled     bool `json:"enabled,omitempty"`
		Concurrency int  `json:"concurrency,omitempty"`
	}

	Policy struct {
		Handshake    int `json:"handshake,omitempty"`
		ConnIdle     int `json:"connIdle,omitempty"`
		UplinkOnly   int `json:"uplinkOnly"`
		DownlinkOnly int `json:"downlinkOnly"`
	}

	Dial  chrome.DialOptions
	Relay chrome.RelayOptions

	ins *v2ray.Instance
}

type ProtocolOptions struct {
	TROJAN struct {
		HostportOptions `yaml:",inline"`
		Password        string
	}
	VLESS struct {
		HostportOptions `yaml:",inline"`
		ID              string
	}
	VMESS struct {
		HostportOptions `yaml:",inline"`
		ID              string
		AlterID         int    `yaml:"aid"`
		Security        string `yaml:"scy"`
	}
}

type TransportOptions struct {
	GRPC struct {
		ServiceName string
	}
	HTTP struct {
		Host []string
		Path string
	}
	TCP struct{}
	TLS struct {
		Enabled       bool                 `json:"-" yaml:"-"`
		ServerName    string               `json:"serverName,omitempty"`
		AllowInsecure bool                 `json:"allowInsecure,omitempty"`
		Certificates  []CertificateOptions `json:"certificates,omitempty" yaml:"-"`
		CertFile      chrome.EnvString     `json:"-"`
	}
	WS struct {
		Path   string
		Header map[string]string
	}
}

type CertificateOptions struct {
	Usage       string   `json:"usage"`
	Certificate []string `json:"certificate"`
}

type HostportOptions struct {
	Address string `yaml:"hostport"`
	Port    int    `yaml:"-"`
}

type Service struct{}

const ServiceName = "v2socks"

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

	var server net.Listener

	startServer := func() error {
		if server != nil {
			return nil
		}

		ln, err := net.Listen("tcp", (<-optsOut).ListenAddr)
		if err != nil {
			logger.Error(err)
			return err
		}

		defer logger.Infof("listening on %v", ln.Addr())

		server = ln

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
					return chrome.ProxyFromDialer(opts.ins), opts.Dial, ok && opts.ins != nil
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

		return nil
	}

	stopServer := func() {
		if server == nil {
			return
		}

		defer logger.Infof("stopped listening on %v", server.Addr())

		_ = server.Close()
		server = nil
	}
	defer stopServer()

	var ins *v2ray.Instance

	startInstance := func(opts Options) {
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
	}

	stopInstance := func() {
		if ins != nil {
			if err := ins.Close(); err != nil {
				logger.Debugf("close instance: %v", err)
			}

			ins = nil
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

				if _, _, err := net.SplitHostPort(new.ListenAddr); err != nil {
					logger.Error(err)
					return
				}

				if new.ListenAddr != old.ListenAddr {
					stopServer()
				}

				if new.TLS.CertFile != "" {
					certLines, err := readLines(ctx.Manager, new.TLS.CertFile.String())
					if err != nil {
						logger.Errorf("read cert file: %v", err)
						return
					}

					if len(certLines) != 0 {
						new.TLS.Certificates = []CertificateOptions{{"verify", certLines}}
					}
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
					startInstance(new)
					new.ins = ins
				}

				optsIn <- new
			}
		case <-ctx.Loaded:
			if err := startServer(); err != nil {
				return
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
	if err := parseURL(&opts); err != nil {
		return nil, err
	}

	opts.Protocol = "VMESS"
	opts.Transport = "TCP"

	for _, t := range strings.SplitN(opts.Type, "+", 3) {
		t = strings.ToUpper(t)
		switch t {
		case "TROJAN", "VLESS", "VMESS":
			opts.Protocol = t
		case "GRPC", "HTTP", "TCP", "WS":
			opts.Transport = t
		case "TLS":
			opts.TLS.Enabled = true
		case "":
		default:
			return nil, fmt.Errorf("unknown type: %v", opts.Type)
		}
	}

	var hostport *HostportOptions

	switch opts.Protocol {
	case "TROJAN":
		hostport = &opts.TROJAN.HostportOptions
	case "VLESS":
		hostport = &opts.VLESS.HostportOptions
	case "VMESS":
		hostport = &opts.VMESS.HostportOptions
	}

	if hostport != nil {
		host, port, err := net.SplitHostPort(hostport.Address)
		if err != nil {
			return nil, fmt.Errorf("invalid address: %v", hostport.Address)
		}

		hostport.Address = host

		hostport.Port, err = strconv.Atoi(port)
		if err != nil {
			return nil, fmt.Errorf("invalid port in address: %v", hostport.Address)
		}
	}

	var buf bytes.Buffer
	if err := v2socksTemplate.Execute(&buf, &opts); err != nil {
		return nil, err
	}

	return v2ray.NewInstanceFromJSON(buf.Bytes())
}

func parseURL(opts *Options) error {
	if opts.URL == "" {
		return nil
	}

	switch {
	case strings.HasPrefix(opts.URL, "trojan://"):
		return parseVLessURL(opts, "trojan://")
	case strings.HasPrefix(opts.URL, "vless://"):
		return parseVLessURL(opts, "vless://")
	case strings.HasPrefix(opts.URL, "vmess://"):
		return parseVMessURL(opts)
	}

	return fmt.Errorf("unknown scheme in url %v", opts.URL)
}

func parseVLessURL(opts *Options, prefix string) error {
	before, after, _ := strings.Cut(strings.TrimPrefix(opts.URL, prefix), "@")
	if before == "" || after == "" {
		return fmt.Errorf("invalid url: %v", opts.URL)
	}

	u, err := url.Parse(prefix + "xxxxx@" + after)
	if err != nil {
		return err
	}

	if u.Port() == "" {
		return fmt.Errorf("missing port: %v", opts.URL)
	}

	q := u.Query()

	switch s := q.Get("encryption"); s {
	case "", "none":
	default:
		return fmt.Errorf("unsupported encryption in url %v: %v", opts.URL, s)
	}

	var transport string

	switch typ := strings.ToUpper(q.Get("type")); typ {
	case "":
		transport = "TCP+TLS"
	case "GRPC":
		transport = "GRPC"
		opts.GRPC.ServiceName = q.Get("serviceName")
	case "TCP":
		transport = "TCP"
		if strings.EqualFold(q.Get("security"), "TLS") {
			transport = "TCP+TLS"
		}
	case "WS":
		transport = "WS"
		if strings.EqualFold(q.Get("security"), "TLS") {
			transport = "WS+TLS"
		}

		opts.WS.Path = q.Get("path")
	default:
		return fmt.Errorf("unknown type in url %v: %v", opts.URL, typ)
	}

	switch transport {
	case "GRPC", "TCP+TLS", "WS+TLS":
		if sni := q.Get("sni"); sni != "" && sni != u.Hostname() {
			opts.TLS.ServerName = sni
		} else if host := q.Get("host"); host != "" && host != u.Hostname() {
			opts.TLS.ServerName = host
		}
	}

	switch prefix {
	case "trojan://":
		opts.Type = "TROJAN+" + transport
		opts.TROJAN.Address = u.Host
		opts.TROJAN.Password = before
	case "vless://":
		opts.Type = "VLESS+" + transport
		opts.VLESS.Address = u.Host
		opts.VLESS.ID = before
	}

	if q.Get("allowInsecure") == "1" {
		opts.TLS.AllowInsecure = true
	}

	return nil
}

func parseVMessURL(opts *Options) error {
	b64 := strings.TrimPrefix(opts.URL, "vmess://")

	b64 = strings.ReplaceAll(b64, "-", "+")
	b64 = strings.ReplaceAll(b64, "_", "/")

	enc := base64.StdEncoding
	if len(b64)%4 != 0 {
		enc = base64.RawStdEncoding
	}

	data, err := enc.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("decode vmess url %v: %w", opts.URL, err)
	}

	var config struct {
		Version json.RawMessage `json:"v"`

		Net  string          `json:"net"`
		TLS  json.RawMessage `json:"tls"`
		Type string          `json:"type"`

		Address  string          `json:"add"`
		Port     json.RawMessage `json:"port"`
		ID       string          `json:"id"`
		AlterID  json.RawMessage `json:"aid"`
		Security string          `json:"scy"`

		Path string `json:"path"`
		Host string `json:"host"`
	}

	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("unmarshal decoded vmess url %v: %w", opts.URL, err)
	}

	if unquote(string(config.Version)) == "" {
		fields := strings.SplitN(config.Path, ";", 2)
		if len(fields) == 2 {
			config.Path, config.Host = fields[0], fields[1]
		}
	}

	var transport string

	switch strings.ToUpper(config.Net) {
	case "GRPC":
		transport = "GRPC"
		opts.GRPC.ServiceName = config.Path
	case "HTTP", "H2":
		transport = "HTTP"

		if config.Host != "" {
			opts.HTTP.Host = []string{config.Host}
			if config.Host != config.Address {
				opts.TLS.ServerName = config.Host
			}
		}

		opts.HTTP.Path = config.Path
	case "TCP":
		transport = "TCP"

		if config.Type != "" && config.Type != "none" {
			return fmt.Errorf("unknown type field in vmess url %v: %v", opts.URL, config.Type)
		}

		if isTLS(unquote(string(config.TLS))) {
			transport = "TCP+TLS"
		}
	case "WS":
		transport = "WS"
		if isTLS(unquote(string(config.TLS))) {
			transport = "WS+TLS"
		}

		opts.WS.Path = config.Path
	default:
		return fmt.Errorf("unknown net field in vmess url %v: %v", opts.URL, config.Net)
	}

	switch transport {
	case "GRPC", "TCP+TLS", "WS+TLS":
		if config.Host != "" && config.Host != config.Address {
			opts.TLS.ServerName = config.Host
		}
	}

	opts.Type = "VMESS+" + transport
	opts.VMESS.Address = net.JoinHostPort(config.Address, unquote(string(config.Port)))
	opts.VMESS.ID = config.ID
	opts.VMESS.AlterID, _ = strconv.Atoi(unquote(string(config.AlterID)))

	if opts.VMESS.Security == "" {
		opts.VMESS.Security = config.Security
	}

	return nil
}

func isTLS(s string) bool {
	return len(s) != 0 && (s[0] == 't' || s[0] == 'T')
}

func unquote(s string) string {
	if s, err := strconv.Unquote(s); err == nil {
		return s
	}

	return s
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
