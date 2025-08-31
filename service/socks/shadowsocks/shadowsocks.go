package shadowsocks

import (
	"context"
	"net"

	"github.com/b97tsk/chrome"
	"github.com/b97tsk/proxy"
	"github.com/shadowsocks/go-shadowsocks2/core"
	"github.com/shadowsocks/go-shadowsocks2/socks"
)

type Options struct {
	ListenAddr string `yaml:"on"`

	Proxy chrome.Proxy `yaml:"over"`

	Method   string
	Password string

	Conn  chrome.ConnOptions
	Relay chrome.RelayOptions

	cipher core.Cipher
}

type Service struct{}

const ServiceName = "shadowsocks"

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

		go ctx.Manager.Serve(ln, func(local net.Conn) {
			cipher := (<-optsOut).cipher
			if cipher == nil {
				return
			}

			local = cipher.StreamConn(local)
			addr, err := socks.ReadAddr(local)
			if err != nil {
				logger.Debugf("read addr: %v", err)
				return
			}

			opts, ok := <-optsOut
			if !ok {
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
		case ev := <-ctx.Event:
			switch ev := ev.(type) {
			case chrome.LoadEvent:
				old := <-optsOut
				new := *ev.Options.(*Options)
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
			case chrome.LoadedEvent:
				if err := startServer(); err != nil {
					return
				}
			}
		}
	}
}
