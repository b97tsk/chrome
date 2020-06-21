package service

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/b97tsk/chrome/internal/proxy"
	"gopkg.in/yaml.v2"
)

type Service interface {
	Name() string
	Run(Context)
	UnmarshalOptions([]byte) (interface{}, error)
}

type Context struct {
	Done       <-chan struct{}
	Events     <-chan interface{}
	Manager    *Manager
	ListenAddr string
}

type Job struct {
	ServiceName string
	Done        <-chan struct{}
	Events      chan<- interface{}
	Cancel      context.CancelFunc
}

func (job *Job) Active() bool {
	select {
	case <-job.Done:
		return false
	default:
		return true
	}
}

func (job *Job) SetOptions(v interface{}) {
	values := []interface{}{v, nil}
	for _, v := range values {
		select {
		case <-job.Done:
			return
		case job.Events <- v:
		}
	}
}

type Manager struct {
	mu          sync.Mutex
	services    map[string]Service
	jobs        map[string]Job
	dialTimeout int64
	connections struct {
		sync.Map
		sync.WaitGroup
	}
}

func NewManager() *Manager {
	return &Manager{
		services: make(map[string]Service),
		jobs:     make(map[string]Job),
	}
}

func (man *Manager) Add(service Service) {
	man.mu.Lock()
	man.services[service.Name()] = service
	man.mu.Unlock()
}

func (man *Manager) setOptions(name string, data interface{}) error {
	if strings.TrimPrefix(name, "alias") != name {
		return nil // If name starts with "alias", silently ignores it.
	}
	fields := strings.SplitN(name, "|", 3)
	if len(fields) != 3 {
		return fmt.Errorf("ignored %v", name)
	}

	serviceName, listenAddr := fields[0], net.JoinHostPort(fields[1], fields[2])
	service, ok := man.services[serviceName]
	if !ok {
		return fmt.Errorf("service %q not found", serviceName)
	}

	text, _ := yaml.Marshal(data)
	options, err := service.UnmarshalOptions(text)
	if err != nil {
		return fmt.Errorf("invalid options in %v", name)
	}

	job, ok := man.jobs[name]
	if !ok || !job.Active() {
		ctx, cancel := context.WithCancel(context.TODO())
		done := make(chan struct{})
		events := make(chan interface{})
		go func() {
			defer func() {
				if err := recover(); err != nil {
					log.Printf(
						"[services] job %q panic: %v\n%v\n",
						name, err, string(debug.Stack()),
					)
				}
			}()
			defer cancel()
			defer close(done)
			service.Run(Context{ctx.Done(), events, man, listenAddr})
		}()
		job = Job{serviceName, done, events, cancel}
		man.jobs[name] = job
	}
	job.SetOptions(options)
	return nil
}

func (man *Manager) Load(configFile string) {
	man.mu.Lock()
	defer man.mu.Unlock()

	file, err := os.Open(configFile)
	if err != nil {
		log.Printf("[services] %v\n", err)
		return
	}
	defer file.Close()

	var c struct {
		Logfile string `yaml:"logging"`
		Dial    struct {
			Timeout time.Duration
		}
		Jobs map[string]interface{} `yaml:",inline"`
	}

	dec := yaml.NewDecoder(file)
	dec.SetStrict(true)

	if err := dec.Decode(&c); err != nil {
		log.Printf("[services] loading %v: %v\n", configFile, err)
		return
	}

	if err := man.setOptions("logging||", c.Logfile); err != nil {
		log.Printf("[services] loading %v: %v\n", configFile, err)
	}

	atomic.StoreInt64(&man.dialTimeout, int64(c.Dial.Timeout))

	for name, data := range c.Jobs {
		if r := reNumberPlus.FindStringIndex(name); r != nil {
			head, tail := name[:r[0]], name[r[1]:]
			s := reNumberPlus.FindStringSubmatch(name[r[0]:r[1]])
			x, _ := strconv.Atoi(s[1])
			if s[2] != "" {
				n, _ := strconv.Atoi(s[2])
				for i := 0; i <= n; i++ {
					c.Jobs[head+strconv.Itoa(x)+tail] = data
					x++
				}
			} else {
				for _, data := range data.([]interface{}) {
					c.Jobs[head+strconv.Itoa(x)+tail] = data
					x++
				}
			}
			delete(c.Jobs, name)
		}
	}

	for name, job := range man.jobs {
		if job.ServiceName == "logging" {
			continue
		}
		if _, ok := c.Jobs[name]; ok {
			continue
		}
		job.Cancel()
		<-job.Done
		delete(man.jobs, name)
	}
	for name, data := range c.Jobs {
		if err := man.setOptions(name, data); err != nil {
			log.Printf("[services] loading %v: %v\n", configFile, err)
		}
	}

	log.Printf("[services] loaded %v\n", configFile)
}

func (man *Manager) ServeListener(ln net.Listener, handle func(net.Conn)) {
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				if isTemporary(err) {
					continue
				}
				return
			}
			man.connections.Store(c, struct{}{})
			man.connections.Add(1)
			go func() {
				defer func() {
					c.Close()
					man.connections.Delete(c)
					man.connections.Done()
				}()
				handle(c)
			}()
		}
	}()
}

func (man *Manager) Shutdown() {
	man.mu.Lock()
	defer man.mu.Unlock()

	for _, job := range man.jobs {
		if job.ServiceName != "logging" {
			job.Cancel()
		}
	}
	for _, job := range man.jobs {
		if job.ServiceName != "logging" {
			<-job.Done
		}
	}

	for _, job := range man.jobs {
		if job.ServiceName == "logging" {
			job.Cancel()
		}
	}
	for _, job := range man.jobs {
		if job.ServiceName == "logging" {
			<-job.Done
		}
	}

	man.connections.Range(func(key, _ interface{}) bool {
		key.(net.Conn).Close()
		return true
	})
	man.connections.Wait()
}

func (man *Manager) Dial(ctx context.Context, d proxy.Dialer, network, address string, timeout time.Duration) (conn net.Conn, err error) {
	if d == nil {
		d = proxy.Direct
	}
	dialTimeout := defaultDialTimeout
	if timeout > 0 {
		dialTimeout = timeout
	} else if timeout := atomic.LoadInt64(&man.dialTimeout); timeout > 0 {
		dialTimeout = time.Duration(timeout)
	}
	for {
		err = ctx.Err()
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(ctx, dialTimeout)
		conn, err = proxy.Dial(ctx, d, network, address)
		cancel()
		if err == nil || !isTemporary(err) {
			return
		}
	}
}

func isTemporary(err error) bool {
	e, ok := err.(interface {
		Temporary() bool
	})
	return ok && e.Temporary()
}

var reNumberPlus = regexp.MustCompile(`(\d+)\+(\d*)`)

const defaultDialTimeout = 30 * time.Second
