package shadowsocks

import (
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

	Proxy chrome.ProxyOptions `yaml:"over"`

	Dial struct {
		Timeout time.Duration
	}
	Relay chrome.RelayOptions

	cipher core.Cipher
}

type Service struct{}

const _ServiceName = "shadowsocks"

func (Service) Name() string {
	return _ServiceName
}

func (Service) Options() interface{} {
	return new(Options)
}

func (Service) Run(ctx chrome.Context) {
	logger := ctx.Manager.Logger(_ServiceName)

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

		opts := <-optsOut

		ln, err := net.Listen("tcp", opts.ListenAddr)
		if err != nil {
			logger.Error(err)
			return err
		}

		defer logger.Infof("listening on %v", ln.Addr())

		server = ln

		go ctx.Manager.Serve(ln, func(c net.Conn) {
			opts := <-optsOut
			if opts.cipher == nil {
				return
			}

			c = opts.cipher.StreamConn(c)
			addr, err := socks.ReadAddr(c)
			if err != nil {
				logger.Debugf("read addr: %v", err)
				return
			}

			local, localCtx := chrome.NewConnChecker(c)

			remote, err := ctx.Manager.Dial(localCtx, opts.Proxy.Dialer(), "tcp", addr.String(), opts.Dial.Timeout)
			if err != nil {
				logger.Trace(err)
				return
			}
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
		case opts := <-ctx.Load:
			if new, ok := opts.(*Options); ok {
				old := <-optsOut
				new := *new
				new.cipher = old.cipher

				if new.ListenAddr != old.ListenAddr {
					stopServer()
				}

				if new.Method != old.Method || new.Password != old.Password {
					cipher, err := core.PickCipher(new.Method, nil, new.Password)
					if err != nil {
						logger.Errorf("fatal: pick cipher: %v", err)
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
