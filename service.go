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
	builtin  struct {
		loggingService
		dialingService
		servingService
	}
}

type fsysValue struct {
	fs.FS
}

func NewManager() *Manager {
	return new(Manager)
}

func (man *Manager) AddService(service Service) {
	man.mu.Lock()

	if man.services == nil {
		man.services = make(map[string]Service)
	}

	man.services[service.Name()] = service

	man.mu.Unlock()
}

func (man *Manager) Open(name string) (fs.File, error) {
	fsys, _ := man.fsys.Load().(fsysValue)
	if fsys.FS == nil {
		return nil, fs.ErrInvalid
	}

	return fsys.Open(name)
}

func (man *Manager) LoadFile(name string) {
	file, err := os.Open(name)
	if err != nil {
		logger := man.Logger("manager")
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
			logger := man.Logger("manager")
			logger.Errorf("LoadFile: open %v in %v: %v", configFile, name, err)

			return
		}

		defer file.Close()

		man.fsys.Store(fsysValue{zr})
		man.loadConfig(file)
		man.fsys.Store(fsysValue{})

		return
	}

	_, _ = file.Seek(0, io.SeekStart)

	man.Load(file)
}

func (man *Manager) Load(r io.Reader) {
	man.fsys.Store(fsysValue{os.DirFS(".")})
	man.loadConfig(r)
}

func (man *Manager) loadConfig(r io.Reader) {
	man.mu.Lock()
	defer man.mu.Unlock()

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
	logger := man.Logger("manager")

	if err := dec.Decode(&config); err != nil {
		logger.Errorf("loadConfig: %v", err)
		return
	}

	if err := man.builtin.SetLogFile(string(config.Log.File)); err != nil {
		logger.Errorf("loadConfig: %v", err)
	}

	man.builtin.SetLogLevel(config.Log.Level)
	man.builtin.SetDialTimeout(config.Dial.Timeout)

	for name, job := range man.jobs {
		if _, ok := config.Jobs[name]; ok {
			continue
		}

		job.Cancel()
		<-job.Done()
		delete(man.jobs, name)
	}

	for name, data := range config.Jobs {
		if err := man.setOptions(name, data); err != nil {
			logger.Errorf("loadConfig: %v", err)
		}
	}

	for _, job := range man.jobs {
		job.sendLoaded()
	}
}

func (man *Manager) setOptions(name string, data interface{}) error {
	serviceName := findServiceName(name)
	if serviceName == "" || serviceName == "alias" {
		return nil
	}

	service, ok := man.services[serviceName]
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

	job, ok := man.jobs[name]
	if !ok || job.Err() != nil {
		ctx1, done := context.WithCancel(context.Background())
		ctx2, cancel := context.WithCancel(ctx1)
		loadChan, loadedChan := make(chan interface{}), make(chan struct{}, 1)
		job = Job{ctx1, cancel, loadChan, loadedChan}

		if man.jobs == nil {
			man.jobs = make(map[string]Job)
		}

		man.jobs[name] = job

		go func() {
			defer func() {
				if err := recover(); err != nil {
					logger := man.Logger("manager")
					logger.Errorf("job %q panic: %v\n%v", name, err, string(debug.Stack()))
				}

				done()
			}()

			service.Run(Context{ctx2, man, loadChan, loadedChan})
		}()
	}

	if opts != nil {
		job.sendOpts(opts)
	}

	return nil
}

func (man *Manager) Shutdown() {
	man.mu.Lock()
	defer man.mu.Unlock()

	for _, job := range man.jobs {
		job.Cancel()
	}

	for _, job := range man.jobs {
		<-job.Done()
	}

	_ = man.builtin.SetLogFile("")
	man.builtin.SetLogOutput(nil)
	man.builtin.CloseConnections()
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
