package main

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"sync"

	"golang.org/x/net/proxy"
	"gopkg.in/yaml.v2"
)

type Service interface {
	Run(ServiceCtx)
	UnmarshalOptions([]byte) (interface{}, error)
}

type ServiceCtx struct {
	Name   string
	Done   <-chan struct{}
	Events <-chan interface{}
}

type ServiceJob struct {
	Done   <-chan struct{}
	Events chan<- interface{}
	Cancel context.CancelFunc
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
	mu       sync.Mutex
	hash     uint32
	proxies  map[string]ProxyList
	services map[string]ServiceItem
}

type ServiceItem struct {
	Name string
	Service
	Jobs map[string]ServiceJob
}

func newServiceManager() *ServiceManager {
	return &ServiceManager{
		proxies:  make(map[string]ProxyList),
		services: make(map[string]ServiceItem),
	}
}

func (sm *ServiceManager) Add(name string, service Service) {
	sm.services[name] = ServiceItem{name, service, nil}
}

func (sm *ServiceManager) setOptions(service, name string, data interface{}) error {
	if service, ok := sm.services[service]; ok {
		text, _ := yaml.Marshal(data)
		options, err := service.UnmarshalOptions(text)
		if err != nil {
			return fmt.Errorf("service %v: invalid options: %v", service.Name, data)
		}
		job, ok := service.Jobs[name]
		if !ok || !job.Active() {
			ctx, cancel := context.WithCancel(context.TODO())
			done := make(chan struct{})
			events := make(chan interface{})
			go func() {
				defer cancel()
				defer close(done)
				service.Run(ServiceCtx{name, ctx.Done(), events})
			}()
			if service.Jobs == nil {
				service.Jobs = make(map[string]ServiceJob)
				sm.services[service.Name] = service
			}
			job = ServiceJob{done, events, cancel}
			service.Jobs[name] = job
		}
		sm.mu.Unlock()
		job.SendData(options)
		sm.mu.Lock()
		return nil
	}
	return fmt.Errorf("service %q not found", service)
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

	data, err := ioutil.ReadAll(file)
	if err != nil {
		log.Printf("[services] loading %v: %v\n", configFile, err)
		return
	}

	hash := crc32.ChecksumIEEE(data)
	if hash == sm.hash {
		return
	}
	sm.hash = hash

	var c struct {
		Logfile string `yaml:"logging"`
		Proxies map[string]ProxyList
		Jobs    map[string]map[string]interface{} `yaml:",inline"`
	}

	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		log.Printf("[services] loading %v: %v\n", configFile, err)
		return
	}

	if err := sm.setOptions("logging", "logging", c.Logfile); err != nil {
		log.Printf("[services] loading %v: %v\n", configFile, err)
	}

	for name, proxies := range sm.proxies {
		if pl, ok := c.Proxies[name]; ok && pl.Equals(proxies) {
			continue
		}
		delete(sm.proxies, name)
	}
	for name, proxies := range c.Proxies {
		sm.proxies[name] = proxies
	}

	for _, service := range sm.services {
		if service.Name == "logging" {
			continue
		}
		for name, job := range service.Jobs {
			if _, ok := c.Jobs[service.Name][name]; ok {
				continue
			}
			job.Cancel()
			<-job.Done
			delete(service.Jobs, name)
		}
	}
	for service, jobs := range c.Jobs {
		for name, data := range jobs {
			if err := sm.setOptions(service, name, data); err != nil {
				log.Printf("[services] loading %v: %v\n", configFile, err)
			}
		}
	}

	log.Printf("[services] loaded %v\n", configFile)
}

func (sm *ServiceManager) Shutdown() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	loggingService := sm.services["logging"]
	delete(sm.services, "logging")

	for _, service := range sm.services {
		for _, job := range service.Jobs {
			job.Cancel()
		}
	}

	for _, service := range sm.services {
		for _, job := range service.Jobs {
			<-job.Done
		}
	}

	for _, job := range loggingService.Jobs {
		job.Cancel()
	}
	for _, job := range loggingService.Jobs {
		<-job.Done
	}
}

type Proxy struct {
	URL *url.URL
	Raw string
}

type ProxyList []Proxy

func (pl ProxyList) Equals(other ProxyList) bool {
	if len(pl) != len(other) {
		return false
	}
	for i, p := range other {
		if p.Raw != pl[i].Raw {
			return false
		}
	}
	return true
}

func (pl ProxyList) Dialer(forward proxy.Dialer) (proxy.Dialer, error) {
	var firstErr error
	for i := len(pl) - 1; i > -1; i-- {
		d, err := proxy.FromURL(pl[i].URL, forward)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		forward = d
	}
	return forward, firstErr
}

func (pl *ProxyList) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var rawurl string
	if err := unmarshal(&rawurl); err == nil {
		u, err := url.Parse(rawurl)
		if err != nil {
			return errors.New("invalid proxy: " + rawurl)
		}
		*pl = []Proxy{{u, rawurl}}
		_, err = pl.Dialer(direct)
		return err
	}
	var slice []string
	if err := unmarshal(&slice); err == nil {
		*pl = nil
		for _, rawurl := range slice {
			u, err := url.Parse(rawurl)
			if err != nil {
				return errors.New("invalid proxy: " + rawurl)
			}
			*pl = append(*pl, Proxy{u, rawurl})
		}
		_, err = pl.Dialer(direct)
		return err
	}
	return errors.New("invalid proxy list")
}

func (sm *ServiceManager) ProxyList(names ...string) (proxies ProxyList) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for _, name := range names {
		pl, ok := sm.proxies[name]
		if !ok {
			log.Printf("[services] proxy %q not found\n", name)
			continue
		}
		proxies = append(proxies, pl...)
	}
	return
}

type ProxyNameList []string

func (sl *ProxyNameList) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var scalar string
	if err := unmarshal(&scalar); err == nil {
		*sl = []string{scalar}
		return nil
	}
	var slice []string
	if err := unmarshal(&slice); err == nil {
		*sl = slice
		return nil
	}
	return errors.New("invalid proxy name list")
}

var services = newServiceManager()
