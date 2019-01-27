package main

import (
	"context"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"net"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"

	"github.com/b97tsk/chrome/internal/utility"
	"gopkg.in/yaml.v2"
)

type Service interface {
	Name() string
	Run(ServiceCtx)
	UnmarshalOptions([]byte) (interface{}, error)
}

type ServiceCtx struct {
	ListenAddr string
	Done       <-chan struct{}
	Events     <-chan interface{}
}

type ServiceJob struct {
	ServiceName string
	Done        <-chan struct{}
	Events      chan<- interface{}
	Cancel      context.CancelFunc
}

func (job *ServiceJob) Active() bool {
	select {
	case <-job.Done:
		return false
	default:
		return true
	}
}

func (job *ServiceJob) SendData(v interface{}) {
	values := []interface{}{v, nil}
	for _, v := range values {
		select {
		case <-job.Done:
			return
		case job.Events <- v:
		}
	}
}

type ServiceManager struct {
	mu          sync.Mutex
	hash        uint32
	services    map[string]Service
	jobs        map[string]ServiceJob
	connections struct {
		sync.Map
		sync.WaitGroup
	}
}

func newServiceManager() *ServiceManager {
	return &ServiceManager{
		services: make(map[string]Service),
		jobs:     make(map[string]ServiceJob),
	}
}

func (sm *ServiceManager) Add(name string, service Service) {
	sm.services[name] = service
}

func (sm *ServiceManager) setOptions(name string, data interface{}) error {
	fields := strings.SplitN(name, "|", 3)
	if len(fields) != 3 {
		return fmt.Errorf("ignore %v", name)
	}

	serviceName, listenAddr := fields[0], net.JoinHostPort(fields[1], fields[2])
	service, ok := sm.services[serviceName]
	if !ok {
		return fmt.Errorf("service %q not found", serviceName)
	}

	serviceName = service.Name()
	fields[0] = serviceName
	name = strings.Join(fields, "|")

	text, _ := yaml.Marshal(data)
	options, err := service.UnmarshalOptions(text)
	if err != nil {
		return fmt.Errorf("invalid options in %v", name)
	}

	job, ok := sm.jobs[name]
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
			service.Run(ServiceCtx{listenAddr, ctx.Done(), events})
		}()
		job = ServiceJob{serviceName, done, events, cancel}
		sm.jobs[name] = job
	}

	sm.mu.Unlock()
	job.SendData(options)
	sm.mu.Lock()

	return nil
}

func (sm *ServiceManager) Load(configFile string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	file, err := os.Open(configFile)
	if err != nil {
		log.Printf("[services] %v\n", err)
		return
	}
	defer file.Close()

	digest := crc32.NewIEEE()
	io.Copy(digest, file)
	hash := digest.Sum32()
	if hash == sm.hash {
		return
	}
	sm.hash = hash

	var c struct {
		Logfile string `yaml:"logging"`
		Aliases []interface{}
		Jobs    map[string]interface{} `yaml:",inline"`
	}

	dec := yaml.NewDecoder(file)
	dec.SetStrict(true)
	file.Seek(0, io.SeekStart)

	if err := dec.Decode(&c); err != nil {
		log.Printf("[services] loading %v: %v\n", configFile, err)
		return
	}

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

	if err := sm.setOptions("logging||", c.Logfile); err != nil {
		log.Printf("[services] loading %v: %v\n", configFile, err)
	}

	for name, job := range sm.jobs {
		if job.ServiceName == "logging" {
			continue
		}
		if _, ok := c.Jobs[name]; ok {
			continue
		}
		job.Cancel()
		<-job.Done
		delete(sm.jobs, name)
	}
	for name, data := range c.Jobs {
		if err := sm.setOptions(name, data); err != nil {
			log.Printf("[services] loading %v: %v\n", configFile, err)
		}
	}

	log.Printf("[services] loaded %v\n", configFile)
}

func (sm *ServiceManager) ServeListener(ln net.Listener, handle func(net.Conn)) {
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				if utility.IsTemporary(err) {
					continue
				}
				return
			}
			utility.TCPKeepAlive(c, direct.KeepAlive)
			sm.connections.Store(c, struct{}{})
			sm.connections.Add(1)
			go func() {
				defer func() {
					c.Close()
					sm.connections.Delete(c)
					sm.connections.Done()
				}()
				handle(c)
			}()
		}
	}()
}

func (sm *ServiceManager) Shutdown() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for _, job := range sm.jobs {
		if job.ServiceName != "logging" {
			job.Cancel()
		}
	}
	for _, job := range sm.jobs {
		if job.ServiceName != "logging" {
			<-job.Done
		}
	}

	for _, job := range sm.jobs {
		if job.ServiceName == "logging" {
			job.Cancel()
		}
	}
	for _, job := range sm.jobs {
		if job.ServiceName == "logging" {
			<-job.Done
		}
	}

	sm.connections.Range(func(key, _ interface{}) bool {
		key.(net.Conn).Close()
		return true
	})
	sm.connections.Wait()
}

var services = newServiceManager()

var reNumberPlus = regexp.MustCompile(`(\d+)\+(\d*)`)
