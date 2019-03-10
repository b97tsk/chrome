package socks

import (
	"bufio"
	"hash/crc32"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
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
	Routes    []Route
	ProxyList service.ProxyList `yaml:"over"`
}

type Route struct {
	File      string
	ProxyList service.ProxyList `yaml:"over"`

	hash     uint32
	dialer   proxy.Dialer
	matchset atomic.Value
}

type patternConfig struct {
	matchAllPorts   bool
	matchThosePorts []string
}

func (r *Route) use(r2 *Route) {
	r.hash = r2.hash
	r.matchset.Store(r2.matchset.Load())
}

func (r *Route) Init() {
	file, err := os.Open(r.File)
	if err != nil {
		log.Printf("[socks] %v\n", err)
		return
	}
	defer file.Close()

	digest := crc32.NewIEEE()
	io.Copy(digest, file)
	file.Seek(0, io.SeekStart)
	hash := digest.Sum32()
	if hash == r.hash {
		return
	}
	r.hash = hash

	var set matchset.MatchSet
	patternConfigs := make(map[string]*patternConfig)

	s := bufio.NewScanner(file)
	for s.Scan() {
		line := s.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		portSuffix := regxMatchThePort.FindString(line)
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
	log.Printf("[socks] loaded %v\n", r.File)
}

func (r *Route) Match(hostport string) bool {
	matchset := r.matchset.Load().(*matchset.MatchSet)
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

func (r *Route) Dial(network, addr string) (net.Conn, error) {
	if r.dialer == nil {
		r.dialer, _ = r.ProxyList.NewDialer(direct)
	}
	return r.dialer.Dial(network, addr)
}

type Service struct{}

func (Service) Name() string {
	return "socks"
}

func (Service) Aliases() []string {
	return []string{"socks5"}
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

		if routes := routes.Load().(*[]Route); routes != nil {
			matches := matches.Load().(*sync.Map)
			if r, ok := matches.Load(hostport); ok {
				dial = r.(*Route).Dial
			} else {
				routes := *routes
				for i := range routes {
					r := &routes[i]
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
			log.Printf("[socks] %v\n", err)
			return
		}
		defer rc.Close()

		if err = utility.Relay(rc, c); err != nil {
			log.Printf("[socks] relay: %v\n", err)
		}
	})

	var (
		options     Options
		watcher     *fsnotify.Watcher
		watchEvents <-chan fsnotify.Event
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
				if !new.ProxyList.Equals(old.ProxyList) {
					d, _ := new.ProxyList.NewDialer(direct)
					dial.Store(d.Dial)
				}
				routes.Store((*[]Route)(nil))
				matches.Store((*sync.Map)(nil))
				if len(new.Routes) != 0 {
					if watcher == nil {
						var err error
						watcher, err = fsnotify.NewWatcher()
						if watcher != nil {
							watchEvents = watcher.Events
						}
						if err != nil {
							log.Printf("[socks] %v\n", err)
						}
					}
					if watcher != nil {
						for i := range old.Routes {
							r := &old.Routes[i]
							watcher.Remove(r.File)
						}
					}
					for i := range new.Routes {
						r := &new.Routes[i]
						r.File = filepath.Clean(os.ExpandEnv(r.File))
						if watcher != nil {
							err := watcher.Add(r.File)
							if err != nil {
								log.Printf("[socks] %v\n", err)
							}
						}
						if i < len(old.Routes) {
							r2 := &old.Routes[i]
							if r.File == r2.File {
								r.use(r2)
								continue
							}
						}
						r.Init()
					}
					matches.Store(&sync.Map{})
					routes.Store(&options.Routes)
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
		case name := <-fileChanges:
			routesChanged := false
			for i := range options.Routes {
				r := &options.Routes[i]
				if r.File == name {
					r.Init()
					routesChanged = true
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

var (
	direct = &net.Dialer{
		Timeout:   configure.Timeout,
		KeepAlive: configure.KeepAlive,
	}

	regxMatchThePort = regexp.MustCompile(`:\d+$`)
)
