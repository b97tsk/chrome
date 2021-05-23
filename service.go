package chrome

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/b97tsk/chrome/internal/log"
	"gopkg.in/yaml.v3"
)

type Service interface {
	Name() string
	Options() interface{}
	Run(Context)
}

type Context struct {
	context.Context
	Manager *Manager
	Load    <-chan interface{}
	Loaded  <-chan struct{}
}

type Job struct {
	context.Context
	Cancel context.CancelFunc
	Load   chan<- interface{}
	Loaded chan<- struct{}
}

func (job *Job) sendOpts(opts interface{}) {
	for _, v := range []interface{}{opts, nil} {
		select {
		case <-job.Done():
			return
		case job.Load <- v:
		}
	}
}

func (job *Job) sendLoaded() {
	select {
	case job.Loaded <- struct{}{}:
	default:
	}
}

type Manager struct {
	mu       sync.Mutex
	services map[string]Service
	jobs     map[string]Job
	fsys     atomic.Value

	loggingService
	dialingService
	servingService
}

type fsysValue struct {
	fs.FS
}

func (m *Manager) AddService(service Service) {
	m.mu.Lock()

	if m.services == nil {
		m.services = make(map[string]Service)
	}

	m.services[service.Name()] = service

	m.mu.Unlock()
}

func (m *Manager) Open(name string) (fs.File, error) {
	fsys, _ := m.fsys.Load().(fsysValue)
	if fsys.FS == nil {
		return nil, fs.ErrInvalid
	}

	return fsys.Open(name)
}

func (m *Manager) LoadFile(name string) {
	file, err := os.Open(name)
	if err != nil {
		logger := m.Logger("manager")
		logger.Errorf("LoadFile: %v", err)

		return
	}
	defer file.Close()

	filesize, _ := file.Seek(0, io.SeekEnd)

	if zr, err := zip.NewReader(file, filesize); err == nil {
		configFile := "chrome.yaml"

		file, err := zr.Open(configFile)
		if err != nil {
			if matches, _ := fs.Glob(zr, "*.yaml"); len(matches) == 1 {
				configFile = matches[0]
				file, err = zr.Open(configFile)
			}
		}

		if err != nil {
			logger := m.Logger("manager")
			logger.Errorf("LoadFile: open %v in %v: %v", configFile, name, err)

			return
		}

		defer file.Close()

		m.fsys.Store(fsysValue{zr})
		m.loadConfig(file)
		m.fsys.Store(fsysValue{})

		return
	}

	_, _ = file.Seek(0, io.SeekStart)

	m.Load(file)
}

func (m *Manager) Load(r io.Reader) {
	m.fsys.Store(fsysValue{os.DirFS(".")})
	m.loadConfig(r)
}

func (m *Manager) loadConfig(r io.Reader) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var config struct {
		Log struct {
			File  EnvString
			Level log.Level
		}
		Dial struct {
			Timeout time.Duration
		}
		Jobs map[string]interface{} `yaml:",inline"`
	}

	dec := yaml.NewDecoder(r)
	logger := m.Logger("manager")

	if err := dec.Decode(&config); err != nil {
		logger.Errorf("loadConfig: %v", err)
		return
	}

	if err := m.SetLogFile(string(config.Log.File)); err != nil {
		logger.Errorf("loadConfig: %v", err)
	}

	m.SetLogLevel(config.Log.Level)
	m.SetDialTimeout(config.Dial.Timeout)

	for name, job := range m.jobs {
		if _, ok := config.Jobs[name]; ok {
			continue
		}

		job.Cancel()
		<-job.Done()
		delete(m.jobs, name)
	}

	for name, data := range config.Jobs {
		if err := m.setOptions(name, data); err != nil {
			logger.Errorf("loadConfig: %v", err)
		}
	}

	for _, job := range m.jobs {
		job.sendLoaded()
	}
}

func (m *Manager) setOptions(name string, data interface{}) error {
	serviceName := findServiceName(name)
	if serviceName == "" || serviceName == "alias" {
		return nil
	}

	service, ok := m.services[serviceName]
	if !ok {
		return fmt.Errorf("%v: service not found", name)
	}

	opts := service.Options()
	if opts != nil {
		byteSlice, _ := yaml.Marshal(data)

		dec := yaml.NewDecoder(bytes.NewReader(byteSlice))
		dec.KnownFields(true)

		if err := dec.Decode(opts); err != nil {
			return fmt.Errorf("%v: parse options: %w", name, err)
		}
	}

	job, ok := m.jobs[name]
	if !ok || job.Err() != nil {
		ctx1, done := context.WithCancel(context.Background())
		ctx2, cancel := context.WithCancel(ctx1)
		loadChan, loadedChan := make(chan interface{}), make(chan struct{}, 1)
		job = Job{ctx1, cancel, loadChan, loadedChan}

		if m.jobs == nil {
			m.jobs = make(map[string]Job)
		}

		m.jobs[name] = job

		go func() {
			defer func() {
				if err := recover(); err != nil {
					logger := m.Logger("manager")
					logger.Errorf("job %q panic: %v\n%v", name, err, string(debug.Stack()))
				}

				done()
			}()

			service.Run(Context{ctx2, m, loadChan, loadedChan})
		}()
	}

	if opts != nil {
		job.sendOpts(opts)
	}

	return nil
}

func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, job := range m.jobs {
		job.Cancel()
	}

	for _, job := range m.jobs {
		<-job.Done()
	}

	_ = m.SetLogFile("")
	m.SetLogOutput(nil)
	m.CloseConnections()
}

func findServiceName(s string) string {
	i := strings.IndexFunc(s, func(r rune) bool {
		return r != '-' && !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if i > 0 {
		return s[:i]
	}

	return s
}
