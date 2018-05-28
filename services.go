package main

import (
	"errors"
	"hash/crc32"
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
	UnmarshalSettings(data []byte) (interface{}, error)
	StartNewJob(name string) Job
}

type Job interface {
	Send(v interface{})
	Stop()
	Done() <-chan struct{}
}

type ServiceManager struct {
	mu       sync.Mutex
	hash     uint32
	proxies  map[string]ProxyList
	services map[string]Service
	jobs     map[JobMapKey]Job
	logging  Job
}

type JobMapKey struct {
	Type string
	Name string
}

func newServiceManager() *ServiceManager {
	return &ServiceManager{
		proxies:  make(map[string]ProxyList),
		services: make(map[string]Service),
		jobs:     make(map[JobMapKey]Job),
	}
}

func (sm *ServiceManager) Add(name string, service Service) {
	sm.services[name] = service
}

func (sm *ServiceManager) Load() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

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

	err = yaml.UnmarshalStrict(data, &c)
	if err != nil {
		log.Printf("[services] loading %v: %v\n", configFileName, err)
		return
	}

	if logging, ok := sm.services["logging"]; ok {
		data, _ := yaml.Marshal(c.Logfile)
		settings, err := logging.UnmarshalSettings(data)
		if err == nil {
			if sm.logging == nil {
				sm.logging = logging.StartNewJob("logging")
			}
			sm.logging.Send(settings)
		} else {
			log.Printf("[services] loading %v: logging %q: %v\n", configFileName, c.Logfile, err)
		}
	} else {
		log.Printf("[services] logging service not found\n")
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

	for key, job := range sm.jobs {
		_, ok := c.Jobs[key.Type][key.Name]
		if !ok {
			job.Stop()
			<-job.Done()
			delete(sm.jobs, key)
		}
	}
	for name, jobs := range c.Jobs {
		service, ok := sm.services[name]
		if !ok {
			log.Printf("[services] loading %v: service %q not found\n", configFileName, name)
			continue
		}
		key := JobMapKey{Type: name}
		for name, data := range jobs {
			data, _ := yaml.Marshal(data)
			settings, err := service.UnmarshalSettings(data)
			if err != nil {
				log.Printf("[services] loading %v: job %q: %v\n", configFileName, name, err)
				continue
			}
			key.Name = name
			job, ok := sm.jobs[key]
			if !ok {
				job = service.StartNewJob(name)
				sm.jobs[key] = job
			}
			sm.mu.Unlock()
			job.Send(settings)
			sm.mu.Lock()
		}
	}

	log.Printf("[services] loaded %v\n", configFileName)
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
