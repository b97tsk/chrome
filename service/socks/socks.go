package socks

import (
	"bufio"
	"errors"
	"hash/crc32"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/b97tsk/chrome/configure"
	"github.com/b97tsk/chrome/internal/matchset"
	"github.com/b97tsk/chrome/internal/proxy"
	"github.com/b97tsk/chrome/internal/utility"
	"github.com/b97tsk/chrome/service"
	"github.com/fsnotify/fsnotify"
	"github.com/shadowsocks/go-shadowsocks2/socks"
	"gopkg.in/yaml.v2"
)

type Options struct {
	Routes []RouteInfo
	Proxy  proxy.ProxyChain `yaml:"over"`
}

type RouteInfo struct {
	File  string
	Proxy proxy.ProxyChain `yaml:"over"`
}

type route struct {
	RouteInfo
	hash     uint32
	dialer   proxy.Dialer
	matchset atomic.Value
}

type patternConfig struct {
	matchAllPorts   bool
	matchThosePorts []string
}

func (r *route) Recycle(r2 *route) {
	r.hash = r2.hash
	r.matchset.Store(r2.matchset.Load())
}

func (r *route) Init() error {
	file, err := os.Open(r.File)
	if err != nil {
		return err
	}
	defer file.Close()

	digest := crc32.NewIEEE()
	io.Copy(digest, file)
	file.Seek(0, io.SeekStart)
	hash := digest.Sum32()
	if hash == r.hash {
		return errNotModified
	}
	r.hash = hash

	var set matchset.MatchSet
	patternConfigs := make(map[string]*patternConfig)

	s := bufio.NewScanner(file)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		portSuffix := regxPortSuffix.FindString(line)
		pattern := line[:len(line)-len(portSuffix)]
		config := patternConfigs[pattern]
		if config == nil {
			config = &patternConfig{}
			patternConfigs[pattern] = config
		}
		if portSuffix == "" {
			config.matchAllPorts = true
			config.matchThosePorts = nil
		} else if !config.matchAllPorts {
			config.matchThosePorts = append(config.matchThosePorts, portSuffix[1:])
		}
		set.Add(pattern, config)
	}
	r.matchset.Store(&set)
	return nil
}

func (r *route) Match(hostport string) bool {
	matchset, _ := r.matchset.Load().(*matchset.MatchSet)
	if matchset == nil {
		return false
	}
	host, port, _ := net.SplitHostPort(hostport)
	for _, c := range matchset.Match(host) {
		config := c.(*patternConfig)
		if config.matchAllPorts {
			return true
		}
		for _, p := range config.matchThosePorts {
			if p == port {
				return true
			}
		}
	}
	return false
}

func (r *route) Dial(network, addr string) (net.Conn, error) {
	if r.dialer == nil {
		r.dialer, _ = r.Proxy.NewDialer(direct)
	}
	return r.dialer.Dial(network, addr)
}

type Service struct{}

func (Service) Name() string {
	return "socks"
}

func (Service) Run(ctx service.Context) {
	ln, err := net.Listen("tcp", ctx.ListenAddr)
	if err != nil {
		log.Printf("[socks] %v\n", err)
		return
	}
	log.Printf("[socks] listening on %v\n", ln.Addr())
	defer log.Printf("[socks] stopped listening on %v\n", ln.Addr())
	defer ln.Close()

	var (
		dial    atomic.Value
		routes  atomic.Value
		matches atomic.Value
	)
	dial.Store(direct.Dial)

	ctx.Manager.ServeListener(ln, func(c net.Conn) {
		addr, err := socks.Handshake(c)
		if err != nil {
			log.Printf("[socks] socks handshake: %v\n", err)
			return
		}

		dial := dial.Load().(func(network, addr string) (net.Conn, error))
		hostport := addr.String()

		if routes, _ := routes.Load().([]*route); routes != nil {
			matches := matches.Load().(*sync.Map)
			if r, ok := matches.Load(hostport); ok {
				dial = r.(*route).Dial
			} else {
				for _, r := range routes {
					if r.Match(hostport) {
						log.Printf("[socks] %v matches %v\n", r.File, hostport)
						dial = r.Dial
						matches.Store(hostport, r)
						break
					}
				}
			}
		}

		rc, err := dial("tcp", hostport)
		if err != nil {
			// log.Printf("[socks] %v\n", err)
			return
		}
		defer rc.Close()

		utility.Relay(rc, c)
	})

	var (
		options     Options
		watcher     *fsnotify.Watcher
		watchEvents <-chan fsnotify.Event
		watchErrors <-chan error
		delayTimers = make(map[string]*time.Timer)
		fileChanges = make(chan string)
	)
	defer func() {
		if watcher != nil {
			watcher.Close()
		}
	}()

	for {
		select {
		case data := <-ctx.Events:
			if new, ok := data.(Options); ok {
				old := options
				options = new
				if !new.Proxy.Equals(old.Proxy) {
					d, _ := new.Proxy.NewDialer(direct)
					dial.Store(d.Dial)
				}
				if !routesEquals(new.Routes, old.Routes) {
					if watcher == nil {
						var err error
						watcher, err = fsnotify.NewWatcher()
						if watcher != nil {
							watchEvents = watcher.Events
							watchErrors = watcher.Errors
						}
						if err != nil {
							log.Printf("[socks] %v\n", err)
						}
					}
					newRoutes := make([]*route, len(new.Routes))
					oldRoutes, _ := routes.Load().([]*route)
					if watcher != nil {
						for _, r := range oldRoutes {
							watcher.Remove(r.File)
						}
					}
					for i, r := range new.Routes {
						r.File = filepath.Clean(os.ExpandEnv(r.File))
						newRoutes[i] = &route{RouteInfo: r}
						if watcher != nil {
							err := watcher.Add(r.File)
							if err != nil {
								log.Printf("[socks] watcher: %v\n", err)
							}
						}
						didRecycle := false
						for _, r2 := range oldRoutes {
							if r.File == r2.File {
								newRoutes[i].Recycle(r2)
								didRecycle = true
								break
							}
						}
						if !didRecycle {
							switch err := newRoutes[i].Init(); err {
							case nil:
								log.Printf("[socks] loaded %v\n", r.File)
							case errNotModified:
							default:
								log.Printf("[socks] fatal: %v\n", err)
								return // Consider fatal here.
							}
						}
					}
					matches.Store(&sync.Map{})
					routes.Store(newRoutes)
				}
			}
		case e := <-watchEvents:
			if e.Op&fsnotify.Write != 0 {
				timer := delayTimers[e.Name]
				if timer == nil {
					timer = time.AfterFunc(time.Second, func() {
						fileChanges <- e.Name
					})
					delayTimers[e.Name] = timer
				} else {
					timer.Reset(time.Second)
				}
			}
		case err := <-watchErrors:
			log.Printf("[socks] watcher: %v\n", err)
		case name := <-fileChanges:
			routes, _ := routes.Load().([]*route)
			routesChanged := false
			for _, r := range routes {
				if r.File == name {
					switch err := r.Init(); err {
					case nil:
						log.Printf("[socks] loaded %v\n", r.File)
						routesChanged = true
					case errNotModified:
					default:
						log.Printf("[socks] reload: %v\n", err)
					}
				}
			}
			delete(delayTimers, name)
			if routesChanged {
				timer := delayTimers["<reset-matches>"]
				if timer == nil {
					timer = time.AfterFunc(time.Second, func() {
						matches.Store(&sync.Map{})
					})
					delayTimers["<reset-matches>"] = timer
				} else {
					timer.Reset(time.Second)
				}
			}
		case <-ctx.Done:
			return
		}
	}
}

func (Service) UnmarshalOptions(text []byte) (interface{}, error) {
	var options Options
	if err := yaml.UnmarshalStrict(text, &options); err != nil {
		return nil, err
	}
	return options, nil
}

func routesEquals(a, b []RouteInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		r1, r2 := &a[i], &b[i]
		if r1.File != r2.File || !r1.Proxy.Equals(r2.Proxy) {
			return false
		}
	}
	return true
}

var (
	direct = &net.Dialer{
		Timeout:   configure.Timeout,
		KeepAlive: configure.KeepAlive,
	}

	errNotModified = errors.New("not modified")

	regxPortSuffix = regexp.MustCompile(`:\d+$`)
)
