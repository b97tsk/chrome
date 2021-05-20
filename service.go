package chrome

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	ListenAddr string
	Manager    *Manager
	Opts       <-chan interface{}
}

type Job struct {
	context.Context
	Cancel context.CancelFunc
	Opts   chan<- interface{}
}

func (job *Job) SendOpts(opts interface{}) {
	for _, v := range []interface{}{opts, nil} {
		select {
		case <-job.Done():
			return
		case job.Opts <- v:
		}
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
		const ConfigFile = "chrome.yaml"

		file, err := zr.Open(ConfigFile)
		if err != nil {
			logger := man.Logger("manager")
			logger.Errorf("LoadFile: open %v in %v: %v", ConfigFile, name, err)

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

	for name, data := range config.Jobs {
		if r := reNumberPlus.FindStringIndex(name); r != nil {
			head, tail := name[:r[0]], name[r[1]:]
			s := reNumberPlus.FindStringSubmatch(name[r[0]:r[1]])
			x, _ := strconv.Atoi(s[1])

			if s[2] != "" {
				n, _ := strconv.Atoi(s[2])
				for i := 0; i <= n; i++ {
					config.Jobs[head+strconv.Itoa(x)+tail] = data
					x++
				}
			} else {
				slice, _ := data.([]interface{})
				for _, data := range slice {
					config.Jobs[head+strconv.Itoa(x)+tail] = data
					x++
				}
			}

			delete(config.Jobs, name)
		}
	}

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
}

func (man *Manager) setOptions(name string, data interface{}) error {
	if strings.HasPrefix(name, "alias") {
		return nil // If name starts with "alias", silently ignores it.
	}

	fields := strings.SplitN(name, "|", 3)
	if len(fields) != 3 {
		return fmt.Errorf("%v: ignored", name)
	}

	serviceName, listenAddr := fields[0], net.JoinHostPort(fields[1], fields[2])

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
		copts := make(chan interface{})
		job = Job{ctx1, cancel, copts}

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

			service.Run(Context{ctx2, listenAddr, man, copts})
		}()
	}

	if opts != nil {
		job.SendOpts(opts)
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

var reNumberPlus = regexp.MustCompile(`(\d+)\+(\d*)`)
