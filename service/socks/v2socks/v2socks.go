package v2socks

import (
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
	Security  string `yaml:"-"`

	ProtocolOptions  `yaml:",inline"`
	TransportOptions `yaml:",inline"`
	SecurityOptions  `yaml:",inline"`

	Mux struct {
		Enabled     bool `json:"enabled,omitempty"`
		Concurrency int  `json:"concurrency,omitempty"`
	}

	Dial  chrome.DialOptions
	Relay chrome.RelayOptions

	ins *v2ray.Instance
}

type ProtocolOptions struct {
	SHADOWSOCKS struct {
		HostportOptions `yaml:",inline"`
		Method          string `json:"method"`
		Password        string `json:"password"`
	}
	SHADOWSOCKS2022 struct {
		HostportOptions `yaml:",inline"`
		Method          string   `json:"method"`
		PSK             string   `json:"psk"`
		IPSK            []string `json:"ipsk,omitempty"`
	}
	TROJAN struct {
		HostportOptions `yaml:",inline"`
		Password        string `json:"password"`
	}
	VLESS, VMESS struct {
		HostportOptions `yaml:",inline"`
		UUID            string `json:"uuid"`
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
		Path   string       `json:"path,omitempty"`
		Header []HeaderItem `json:"header,omitempty"`
	}
}

type HeaderItem struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type SecurityOptions struct {
	TLS struct {
		ServerName  string               `json:"serverName,omitempty"`
		Certificate []CertificateOptions `json:"certificate,omitempty" yaml:"-"`
		CertFile    chrome.EnvString     `json:"-"`
	}
}

type CertificateOptions struct {
	Usage       string `json:"usage"`
	Certificate string `json:"certificate"`
}

type HostportOptions struct {
	Address string `json:"address" yaml:"hostport"`
	Port    int    `json:"port" yaml:"-"`
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
		case ev := <-ctx.Event:
			switch ev := ev.(type) {
			case chrome.LoadEvent:
				old := <-optsOut
				new := *ev.Options.(*Options)
				new.ins = old.ins

				if _, _, err := net.SplitHostPort(new.ListenAddr); err != nil {
					logger.Error(err)
					return
				}

				if new.ListenAddr != old.ListenAddr {
					stopServer()
				}

				if new.TLS.CertFile != "" {
					certData, err := fs.ReadFile(ctx.Manager, new.TLS.CertFile.String())
					if err != nil {
						logger.Errorf("read cert file: %v", err)
						return
					}

					if len(certData) == 0 {
						logger.Errorf("empty file: %v", new.TLS.CertFile)
						return
					}

					new.TLS.Certificate = []CertificateOptions{{"AUTHORITY_VERIFY", string(certData)}}
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
			case chrome.LoadedEvent:
				if err := startServer(); err != nil {
					return
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
	if err := parseURL(&opts); err != nil {
		return nil, err
	}

	opts.Protocol = "VMESS"
	opts.Transport = "TCP"

	for _, t := range strings.SplitN(opts.Type, "+", 3) {
		t = strings.ToUpper(t)
		switch t {
		case "SHADOWSOCKS", "SHADOWSOCKS2022", "TROJAN", "VLESS", "VMESS":
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

	var hostport *HostportOptions

	switch opts.Protocol {
	case "SHADOWSOCKS":
		hostport = &opts.SHADOWSOCKS.HostportOptions
	case "SHADOWSOCKS2022":
		hostport = &opts.SHADOWSOCKS2022.HostportOptions
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

	switch opts.Protocol {
	case "SHADOWSOCKS":
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
	case "SHADOWSOCKS2022":
		switch method := normalizeMethod(opts.SHADOWSOCKS2022.Method); method {
		case "2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm":
			opts.SHADOWSOCKS2022.Method = method
		default:
			return nil, fmt.Errorf("unknown method: %v", orEmpty(opts.SHADOWSOCKS2022.Method))
		}
	}

	var buf bytes.Buffer
	if err := v2socksTemplate.Execute(&buf, &opts); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func parseURL(opts *Options) error {
	if opts.URL == "" {
		return nil
	}

	switch {
	case strings.HasPrefix(opts.URL, "ss://"):
		return parseShadowsocksURL(opts)
	case strings.HasPrefix(opts.URL, "trojan://"):
		return parseVLessURL(opts, "trojan://")
	case strings.HasPrefix(opts.URL, "vless://"):
		return parseVLessURL(opts, "vless://")
	case strings.HasPrefix(opts.URL, "vmess://"):
		return parseVMessURL(opts)
	}

	return fmt.Errorf("unknown scheme in url %v", opts.URL)
}

func parseShadowsocksURL(opts *Options) error {
	u, err := url.Parse(opts.URL)
	if err != nil {
		return err
	}

	if u.User == nil {
		data, err := decodeBase64String(u.Host)
		if err != nil {
			return fmt.Errorf("invalid url: %v", opts.URL)
		}

		u, _ = url.Parse(u.Scheme + "://" + string(data))
		if u == nil || u.User == nil {
			return fmt.Errorf("invalid url: %v", opts.URL)
		}
	}

	method := u.User.Username()
	password, ok := u.User.Password()
	if !ok {
		data, err := decodeBase64String(method)
		if err != nil {
			return fmt.Errorf("invalid url: %v", opts.URL)
		}

		method, password, ok = strings.Cut(string(data), ":")
		if !ok {
			return fmt.Errorf("invalid url: %v", opts.URL)
		}
	}

	switch normalizeMethod(method) {
	case "aes-128-gcm", "aes-256-gcm", "chacha20-poly1305", "chacha20-ietf-poly1305":
		opts.Type = "SHADOWSOCKS"
		opts.SHADOWSOCKS.Address = u.Host
		opts.SHADOWSOCKS.Method = method
		opts.SHADOWSOCKS.Password = password
	case "2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm":
		opts.Type = "SHADOWSOCKS2022"
		opts.SHADOWSOCKS2022.Address = u.Host
		opts.SHADOWSOCKS2022.Method = method
		opts.SHADOWSOCKS2022.PSK = password
	default:
		return fmt.Errorf("unknown method in url %v: %v", opts.URL, method)
	}

	return nil
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
		transport = "GRPC+TLS"
		opts.GRPC.ServiceName = q.Get("serviceName")
	case "HTTPUPGRADE":
		transport = "HTTPUPGRADE"

		var sni string

		if strings.EqualFold(q.Get("security"), "TLS") {
			transport = "HTTPUPGRADE+TLS"
			sni = q.Get("sni")
		}

		opts.HTTPUPGRADE.Path = q.Get("path")

		if host := q.Get("host"); host != "" && host != sni && host != u.Hostname() {
			opts.HTTPUPGRADE.Host = host
		}
	case "TCP":
		transport = "TCP"
		if strings.EqualFold(q.Get("security"), "TLS") {
			transport = "TCP+TLS"
		}
	case "WS":
		transport = "WS"

		var sni string

		if strings.EqualFold(q.Get("security"), "TLS") {
			transport = "WS+TLS"
			sni = q.Get("sni")
		}

		opts.WS.Path = q.Get("path")

		if host := q.Get("host"); host != "" && host != sni && host != u.Hostname() {
			opts.WS.Header = append(opts.WS.Header, HeaderItem{"Host", host})
		}
	default:
		return fmt.Errorf("unknown type in url %v: %v", opts.URL, typ)
	}

	switch transport {
	case "GRPC+TLS", "HTTPUPGRADE+TLS", "TCP+TLS", "WS+TLS":
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
		opts.VLESS.UUID = before
	case "vmess://":
		opts.Type = "VMESS+" + transport
		opts.VMESS.Address = u.Host
		opts.VMESS.UUID = before
	}

	return nil
}

func parseVMessURL(opts *Options) error {
	if strings.Contains(opts.URL, "@") {
		return parseVLessURL(opts, "vmess://")
	}

	data, err := decodeBase64String(strings.TrimPrefix(opts.URL, "vmess://"))
	if err != nil {
		return fmt.Errorf("invalid url: %v", opts.URL)
	}

	var config struct {
		Net  string          `json:"net"`
		TLS  json.RawMessage `json:"tls"`
		Type string          `json:"type"`

		Address string          `json:"add"`
		Port    json.RawMessage `json:"port"`
		UUID    string          `json:"id"`

		Path string `json:"path"`
		Host string `json:"host"`
	}

	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("unmarshal decoded vmess url %v: %w", opts.URL, err)
	}

	var transport string

	switch strings.ToUpper(config.Net) {
	case "GRPC":
		transport = "GRPC+TLS"
		opts.GRPC.ServiceName = config.Path
	case "HTTPUPGRADE":
		transport = "HTTPUPGRADE"
		if isTLS(unquote(string(config.TLS))) {
			transport = "HTTPUPGRADE+TLS"
		}

		opts.HTTPUPGRADE.Path = config.Path

		if config.Host != "" && config.Host != config.Address {
			opts.HTTPUPGRADE.Host = config.Host
		}
	case "TCP":
		transport = "TCP"
		if isTLS(unquote(string(config.TLS))) {
			transport = "TCP+TLS"
		}

		if config.Type != "" && config.Type != "none" {
			return fmt.Errorf("unknown type field in vmess url %v: %v", opts.URL, config.Type)
		}
	case "WS":
		transport = "WS"
		if isTLS(unquote(string(config.TLS))) {
			transport = "WS+TLS"
		}

		opts.WS.Path = config.Path

		if config.Host != "" && config.Host != config.Address {
			opts.WS.Header = append(opts.WS.Header, HeaderItem{"Host", config.Host})
		}
	default:
		return fmt.Errorf("unknown net field in vmess url %v: %v", opts.URL, config.Net)
	}

	switch transport {
	case "GRPC+TLS", "HTTPUPGRADE+TLS", "TCP+TLS", "WS+TLS":
		if config.Host != "" && config.Host != config.Address {
			opts.TLS.ServerName = config.Host
		}
	}

	opts.Type = "VMESS+" + transport
	opts.VMESS.Address = net.JoinHostPort(config.Address, unquote(string(config.Port)))
	opts.VMESS.UUID = config.UUID

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

func decodeBase64String(s string) ([]byte, error) {
	s = strings.ReplaceAll(s, "-", "+")
	s = strings.ReplaceAll(s, "_", "/")

	enc := base64.StdEncoding
	if len(s)%4 != 0 {
		enc = base64.RawStdEncoding
	}

	return enc.DecodeString(s)
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
