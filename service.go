package chrome

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
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

	"github.com/b97tsk/log"
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
	relayService
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

type osfs struct{}

func (osfs) Open(name string) (fs.File, error) { return os.Open(name) }

func (m *Manager) Load(r io.Reader) error { return m.load(osfs{}, r) }

func (m *Manager) LoadFile(name string) error { return m.LoadFS(osfs{}, name) }

func (m *Manager) LoadFS(fsys fs.FS, name string) error {
	file, err := fsys.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()

	readerAt, ok := file.(io.ReaderAt)
	if !ok {
		return m.load(fsys, file)
	}

	stat, err := file.Stat()
	if err != nil {
		return m.load(fsys, file)
	}

	zr, err := zip.NewReader(readerAt, stat.Size())
	if err != nil {
		return m.load(fsys, file)
	}

	configFile := "chrome.yaml"

	zf, err := zr.Open(configFile)
	if err != nil {
		if matches, _ := fs.Glob(zr, "*.yaml"); len(matches) == 1 {
			configFile = matches[0]
			zf, err = zr.Open(configFile)
		}
	}

	if err != nil {
		return err
	}

	defer zf.Close()

	return m.load(zr, zf)
}

func (m *Manager) load(fsys fs.FS, file io.Reader) error {
	m.fsys.Store(fsysValue{fsys})
	err := m.loadConfig(file)
	m.fsys.Store(fsysValue{})

	return err
}

func (m *Manager) loadConfig(r io.Reader) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var config struct {
		Log struct {
			File  EnvString
			Level logLevel
		}
		Dial struct {
			Timeout time.Duration
		}
		Relay RelayOptions
		Jobs  map[string]interface{} `yaml:",inline"`
	}

	dec := yaml.NewDecoder(r)
	if err := dec.Decode(&config); err != nil {
		return err
	}

	logger := m.Logger("manager")

	if err := m.SetLogFile(string(config.Log.File)); err != nil {
		logger.Errorf("load config: %v", err)
	}

	m.SetLogLevel(config.Log.Level.Level)
	m.SetDialTimeout(config.Dial.Timeout)
	m.SetRelayOptions(config.Relay)

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
			logger.Errorf("load config: %v", err)
		}
	}

	for _, job := range m.jobs {
		job.sendLoaded()
	}

	return nil
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
					logger.Errorf("job %q: panic: %v\n%v", name, err, string(debug.Stack()))
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

type logLevel struct {
	log.Level
}

func (lv *logLevel) UnmarshalYAML(v *yaml.Node) error {
	var s string

	if err := v.Decode(&s); err != nil {
		return err
	}

	switch {
	case strings.EqualFold(s, log.LevelNone.String()):
		lv.Level = log.LevelNone
	case strings.EqualFold(s, log.LevelError.String()):
		lv.Level = log.LevelError
	case strings.EqualFold(s, log.LevelWarn.String()):
		lv.Level = log.LevelWarn
	case strings.EqualFold(s, log.LevelInfo.String()):
		lv.Level = log.LevelInfo
	case strings.EqualFold(s, log.LevelDebug.String()):
		lv.Level = log.LevelDebug
	case strings.EqualFold(s, log.LevelTrace.String()):
		lv.Level = log.LevelTrace
	default:
		return errors.New("unknown logging level: " + s)
	}

	return nil
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
