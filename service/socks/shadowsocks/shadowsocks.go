package shadowsocks

import (
	"context"
	"net"
	"time"

	"github.com/b97tsk/chrome"
	"github.com/shadowsocks/go-shadowsocks2/core"
	"github.com/shadowsocks/go-shadowsocks2/socks"
)

type Options struct {
	ListenAddr string `yaml:"on"`

	Method   string
	Password string

	Proxy chrome.Proxy `yaml:"over"`

	Dial struct {
		Timeout time.Duration
	}
	Relay chrome.RelayOptions

	cipher core.Cipher
}

type Service struct{}

const ServiceName = "shadowsocks"

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
			cipher := (<-optsOut).cipher
			if cipher == nil {
				return
			}

			c = cipher.StreamConn(c)
			addr, err := socks.ReadAddr(c)
			if err != nil {
				logger.Debugf("read addr: %v", err)
				return
			}

			remoteAddr := addr.String()

			getRemote := func(localCtx context.Context) net.Conn {
				opts, ok := <-optsOut
				if !ok {
					return nil
				}

				remote, err := ctx.Manager.Dial(localCtx, opts.Proxy.Dialer(), "tcp", remoteAddr, opts.Dial.Timeout)
				if err != nil && err != context.Canceled {
					logger.Tracef("dial %v: %v", remoteAddr, err)
				}

				return remote
			}

			ctx.Manager.Relay(c, getRemote, nil, (<-optsOut).Relay)
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

	for {
		select {
		case <-ctx.Done():
			return
		case opts := <-ctx.Load:
			if new, ok := opts.(*Options); ok {
				old := <-optsOut
				new := *new
				new.cipher = old.cipher

				if _, _, err := net.SplitHostPort(new.ListenAddr); err != nil {
					logger.Error(err)
					return
				}

				if new.ListenAddr != old.ListenAddr {
					stopServer()
				}

				if new.Method != old.Method || new.Password != old.Password {
					cipher, err := core.PickCipher(new.Method, nil, new.Password)
					if err != nil {
						logger.Errorf("pick cipher: %v", err)
						return
					}

					new.cipher = cipher
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
