package main

import (
	"errors"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"sync"

	"golang.org/x/net/proxy"
	"gopkg.in/yaml.v2"
)

const configFileName = "chrome.yaml"

type Service interface {
	Type() string
	UnmarshalSettings(data []byte) (interface{}, error)
	StartNewJob(name string) Job
}

type Job interface {
	Type() string
	Send(v interface{})
	Stop()
	Done() <-chan struct{}
}

type ServiceManager struct {
	mu       sync.Mutex
	proxies  map[string]ProxyList
	services map[string]Service
	jobs     map[string]Job
	logging  Job
}

func NewServiceManager() *ServiceManager {
	return &ServiceManager{
		proxies:  make(map[string]ProxyList),
		services: make(map[string]Service),
		jobs:     make(map[string]Job),
	}
}

func (sm *ServiceManager) Add(s Service) {
	sm.services[s.Type()] = s
}

func (sm *ServiceManager) Load() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	log.Printf("[services] loading %v\n", configFileName)

	file, err := os.Open(configFileName)
	if err != nil {
		log.Printf("[services] %v\n", err)
		return
	}
	defer file.Close()

	data, err := ioutil.ReadAll(file)
	if err != nil {
		log.Printf("[services] loading %v: %v\n", configFileName, err)
		return
	}

	var c struct {
		Logfile string `yaml:"logging"`
		Proxies map[string]ProxyList
		Jobs    map[string]JobItem
	}

	err = yaml.Unmarshal(data, &c)
	if err != nil {
		log.Printf("[services] loading %v: %v\n", configFileName, err)
		return
	}

	if logging, ok := sm.services["Logging"]; ok {
		data, _ := yaml.Marshal(c.Logfile)
		settings, err := logging.UnmarshalSettings(data)
		if err == nil {
			if sm.logging == nil {
				sm.logging = logging.StartNewJob("Logging")
			}
			sm.logging.Send(settings)
		} else {
			log.Printf("[services] loading %v: logging %q: %v\n", configFileName, c.Logfile, err)
		}
	} else {
		log.Printf("[services] Logging service not found\n")
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

	for name, job := range sm.jobs {
		if item, ok := c.Jobs[name]; ok {
			if item.Type == job.Type() {
				continue
			}
		}
		job.Stop()
		<-job.Done()
		delete(sm.jobs, name)
	}
	for name, item := range c.Jobs {
		service, ok := sm.services[item.Type]
		if !ok {
			log.Printf("[services] loading %v: service %q not found\n", configFileName, item.Type)
			continue
		}
		data, _ := yaml.Marshal(item.Value)
		settings, err := service.UnmarshalSettings(data)
		if err != nil {
			log.Printf("[services] loading %v: job %q: %v\n", configFileName, name, err)
			continue
		}
		job, ok := sm.jobs[name]
		if !ok {
			job = service.StartNewJob(name)
			sm.jobs[name] = job
		}
		sm.mu.Unlock()
		job.Send(settings)
		sm.mu.Lock()
	}
}

func (sm *ServiceManager) Shutdown() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for _, job := range sm.jobs {
		job.Stop()
	}

	for _, job := range sm.jobs {
		<-job.Done()
	}

	if sm.logging != nil {
		sm.logging.Stop()
		<-sm.logging.Done()
	}
}

type Proxy struct {
	*url.URL
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

func (pl ProxyList) Dialer(forward proxy.Dialer) (dialer proxy.Dialer) {
	dialer = forward
	for i := len(pl) - 1; i > -1; i-- {
		d, err := proxy.FromURL(pl[i].URL, dialer)
		if err != nil {
			log.Printf("[services] proxy %q not recognized\n", pl[i])
			continue
		}
		dialer = d
	}
	return
}

func (pl *ProxyList) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var rawurl string
	if err := unmarshal(&rawurl); err == nil {
		u, err := url.Parse(rawurl)
		if err != nil {
			return errors.New("invalid proxy: " + rawurl)
		}
		*pl = []Proxy{{u, rawurl}}
		return nil
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
		return nil
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

type JobItem struct {
	Type  string
	Value map[string]interface{}
}

func (ji *JobItem) UnmarshalYAML(unmarshal func(interface{}) error) error {
	if err := unmarshal(&ji.Value); err != nil {
		return err
	}
	if t, ok := ji.Value["type"].(string); ok {
		ji.Type = t
		return nil
	}
	return errors.New("invalid job: type not found")
}

var services = NewServiceManager()
