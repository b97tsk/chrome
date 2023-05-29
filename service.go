// Package chrome is a library that can manage and start various services
// implemented in Go (currently, very TCP centric).
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
	"unicode"

	"github.com/b97tsk/log"
	"gopkg.in/yaml.v3"
)

// A Service is something that does certain jobs.
type Service interface {
	// Name gets the name of the Service.
	// Successive calls must return the same value.
	Name() string
	// Options returns a new options for unmarshaling.
	// Options may return nil to indicate no options.
	// If non-nil, the returned value must be a pointer to a struct.
	// The returned value is sent to Context.Load later after unmarshaling.
	Options() any
	// Run starts a job.
	Run(Context)
}

// A Context provides contextual values for a job.
type Context struct {
	// Context is for cancellation. It is canceled when the job cancels.
	context.Context
	// Manager is the Manager that starts the job.
	Manager *Manager
	// Load is for receiving options. The job should only parse the options
	// but not start doing the work, not until the job receives a value from
	// Loaded.
	//
	// If the job has acquired some system resources (for example, listening to
	// a port), this is the good chance to release the resources, so other jobs
	// can acquire the resources.
	Load <-chan any
	// Loaded means that the job now can start doing the work.
	Loaded <-chan struct{}
}

// A Job provides mechanism to control the job started by a Service.
type Job struct {
	// Context is for cancellation. It is canceled when the job stops.
	context.Context
	// Cancel cancels the job and later cancels Context after Service.Run returns.
	Cancel context.CancelFunc
	// Load is for sending options to the job.
	Load chan<- any
	// Loaded is for signaling that the job now can start doing the work.
	Loaded chan<- struct{}
}

// SendOptions sends opts to Load.
func (job Job) SendOptions(opts any) {
	if job.Context == nil {
		return
	}

	select {
	case <-job.Done():
		return
	case job.Load <- opts:
	}
}

// SendLoaded sends a value to Loaded.
func (job Job) SendLoaded() {
	if job.Context == nil {
		return
	}

	select {
	case <-job.Done():
	case job.Loaded <- struct{}{}:
	}
}

// Stop stops the job.
func (job Job) Stop() {
	if job.Context == nil {
		return
	}

	job.Cancel()
	<-job.Done()
}

// A Manager manages services and jobs started by services.
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

// Dummy is a dummy Service.
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

// Service gets the Service that was registered by AddService.
// Service returns nil if there is no registered Service with this name.
// Service returns Dummy if name is not considered a real Service name.
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

// AddService registers a Service. Registering services are important for
// Load(File|FS) methods. Unregistered services cannot be started by
// Load(File|FS) methods.
func (m *Manager) AddService(service Service) {
	m.services.Store(service.Name(), service)
}

// StartService starts a Service. Jobs started by StartService are not managed
// but they should be stopped manually before you Shutdown the Manager since
// somehow they are connected to the Manager.
func (m *Manager) StartService(ctx context.Context, service Service) (Job, error) {
	if service == nil || service == Dummy {
		return Job{}, os.ErrInvalid
	}

	ctx, cancel := context.WithCancel(ctx)
	ctx1, done := context.WithCancel(context.Background())
	load, loaded := make(chan any), make(chan struct{})

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

// Open implements fs.FS. Open is available for jobs started by Load(File|FS)
// methods when they are handling options.
func (m *Manager) Open(name string) (fs.File, error) {
	fsys, _ := m.fsys.Load().(*fs.FS)
	if fsys == nil {
		return nil, fs.ErrInvalid
	}

	return (*fsys).Open(name)
}

type osfs struct{}

func (osfs) Open(name string) (fs.File, error) { return os.Open(name) }

// Load loads a YAML document from r.
func (m *Manager) Load(r io.Reader) error { return m.load(osfs{}, r) }

// LoadFile loads a YAML document from a file.
func (m *Manager) LoadFile(name string) error { return m.LoadFS(osfs{}, name) }

// LoadFS loads a YAML document from a file in a file system.
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
		Dial  DialOptions
		Relay RelayOptions
		Jobs  map[string]any `yaml:",inline"`
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
	m.SetDialOptions(config.Dial)
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

func (m *Manager) setOptions(name string, data any) error {
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

// StopJobs stops managed jobs that were started by Load(File|FS) methods.
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

// Shutdown shutdowns and cleanups the Manager.
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
