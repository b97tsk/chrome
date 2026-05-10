package roundrobin

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net"
	"slices"
	"sync/atomic"

	"github.com/b97tsk/chrome"
	"github.com/b97tsk/proxy"
	"github.com/shadowsocks/go-shadowsocks2/socks"
)

type Options struct {
	ListenAddr string `yaml:"on"`

	Candidates []chrome.Proxy

	Conn  chrome.ConnOptions
	Relay chrome.RelayOptions

	index *atomic.Uint32
}

type Service struct{}

const ServiceName = "roundrobin"

func (Service) Name() string {
	return ServiceName
}

func (Service) Options() any {
	return new(Options)
}

func (Service) Run(ctx chrome.Context) {
	logger := ctx.Manager.Logger().With(slog.String("job", ctx.JobName))

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
			logger.Error("net:listen", slog.Any("error", err))
			return err
		}

		defer logger.Info("net:listening", slog.Any("addr", ln.Addr()))

		server = ln

		go ctx.Manager.Serve(ln, func(local net.Conn) {
			addr, err := socks.Handshake(local)
			if err != nil {
				return
			}

			opts, ok := <-optsOut
			if !ok {
				return
			}

			remoteAddr := addr.String()

			var index *atomic.Uint32
			var indexLoaded uint32

			getRemote := func(ctx context.Context) (net.Conn, error) {
				opts := <-optsOut
				n := len(opts.Candidates)
				if n == 0 {
					return nil, chrome.CloseConn
				}
				if index == nil || index != opts.index {
					index = opts.index
					i := index.Load()
					for !index.CompareAndSwap(i, (i+1)%uint32(n)) {
						i = index.Load()
					}
					indexLoaded = i
				} else {
					indexLoaded = (indexLoaded + 1) % uint32(n)
				}
				d := opts.Candidates[indexLoaded].Dialer()
				return proxy.Dial(ctx, d, "tcp", remoteAddr)
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

		defer logger.Info("net:listen:close", slog.Any("addr", server.Addr()))

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

				if _, _, err := net.SplitHostPort(new.ListenAddr); err != nil {
					logger.Error("loading", slog.Any("error", err))
					return
				}

				if n := len(new.Candidates); n != 0 {
					if slices.EqualFunc(new.Candidates, old.Candidates, chrome.Proxy.Equal) {
						new.index = old.index
					} else {
						new.index = &atomic.Uint32{}
						new.index.Store(uint32(rand.IntN(n)))
					}
				}

				if new.ListenAddr != old.ListenAddr {
					stopServer()
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
