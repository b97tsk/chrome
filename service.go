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

func (job Job) SendOptions(opts interface{}) {
	if job.Context == nil {
		return
	}

	select {
	case <-job.Done():
		return
	case job.Load <- opts:
	}
}

func (job Job) SendLoaded() {
	if job.Context == nil {
		return
	}

	select {
	case <-job.Done():
	case job.Loaded <- struct{}{}:
	}
}

func (job Job) Stop() {
	if job.Context == nil {
		return
	}

	job.Cancel()
	<-job.Done()
}

type Manager struct {
	mu       sync.Mutex
	services sync.Map
	jobs     map[string]Job
	fsys     atomic.Value

	loggingService
	dialingService
	servingService
	relayService
}

var Dummy Service = new(struct{ Service })

func isDummyService(name string) bool {
	return name == "" || name == "alias" || name == "dummy"
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

func (m *Manager) Service(name string) Service {
	name = findServiceName(name)
	if isDummyService(name) {
		return Dummy
	}

	if s, ok := m.services.Load(name); ok {
		return s.(Service)
	}

	return nil
}

func (m *Manager) AddService(service Service) {
	m.services.Store(service.Name(), service)
}

func (m *Manager) StartService(ctx context.Context, service Service) (Job, error) {
	if service == nil || service == Dummy {
		return Job{}, os.ErrInvalid
	}

	ctx, cancel := context.WithCancel(ctx)
	ctx1, done := context.WithCancel(context.Background())
	load, loaded := make(chan interface{}), make(chan struct{})

	go func() {
		defer func() {
			if err := recover(); err != nil {
				logger := m.Logger("manager")
				logger.Errorf("panic: %v\n%v", err, string(debug.Stack()))
			}

			cancel()
			done()
		}()

		service.Run(Context{ctx, m, load, loaded})
	}()

	return Job{ctx1, cancel, load, loaded}, nil
}

func (m *Manager) Open(name string) (fs.File, error) {
	fsys, _ := m.fsys.Load().(*fs.FS)
	if fsys == nil {
		return nil, fs.ErrInvalid
	}

	return (*fsys).Open(name)
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
	m.fsys.Store(&fsys)
	err := m.loadConfig(file)
	m.fsys.Store((*fs.FS)(nil))

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
		job.SendLoaded()
	}

	return nil
}

func (m *Manager) setOptions(name string, data interface{}) error {
	service := m.Service(name)
	switch service {
	case nil:
		return fmt.Errorf("service not found: %v", name)
	case Dummy:
		return nil // Ignore dummy services.
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
		job, _ = m.StartService(context.Background(), service)

		if m.jobs == nil {
			m.jobs = make(map[string]Job)
		}

		m.jobs[name] = job
	}

	if opts != nil {
		job.SendOptions(opts)
		// m.fsys is not always available for jobs.
		// Jobs can only access m.fsys (via m.Open) while handling opts.
		job.SendOptions(nil) // Make sure opts is handled.
	}

	return nil
}

func (m *Manager) StopJobs() {
	m.mu.Lock()

	for _, job := range m.jobs {
		job.Cancel()
	}

	for _, job := range m.jobs {
		<-job.Done()
	}

	m.mu.Unlock()
}

func (m *Manager) Shutdown() {
	m.StopJobs()
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
