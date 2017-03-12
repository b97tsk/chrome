package main

import (
	"context"
	"io"
	"log"
	"os"

	"gopkg.in/yaml.v2"
)

const loggingTypeName = "Logging"

type loggingSettings struct {
	Logfile string
}

type loggingJob struct {
	name   string
	event  chan interface{}
	done   chan struct{}
	cancel context.CancelFunc
}

func (job *loggingJob) start() {
	// log.Printf("[%v] started\n", job.name)

	ctx, cancel := context.WithCancel(context.TODO())
	job.cancel = cancel

	go func() {
		defer close(job.done)
		defer log.Printf("[%v] stopped\n", job.name)

		var settings loggingSettings
		var logfile *os.File
		defer func() {
			if logfile != nil {
				log.SetOutput(os.Stdout)
				// log.Printf("[%v] closing %v\n", job.name, settings.Logfile)
				logfile.Close()
			}
		}()

		for {
			select {
			case v := <-job.event:
				if s, ok := v.(loggingSettings); ok {
					settings, s = s, settings
					if settings.Logfile != s.Logfile {
						if settings.Logfile == "" {
							if logfile != nil {
								log.SetOutput(os.Stdout)
								// log.Printf("[%v] closing %v\n", job.name, s.Logfile)
								logfile.Close()
								logfile = nil
							}
						} else {
							log.Printf("[%v] opening %v\n", job.name, settings.Logfile)
							file, err := os.OpenFile(settings.Logfile, os.O_APPEND|os.O_CREATE, 0644)
							if err != nil {
								log.Printf("[%v] %v", job.name, err)
							} else {
								log.SetOutput(io.MultiWriter(file, os.Stdout))
								if logfile != nil {
									// log.Printf("[%v] closing %v\n", job.name, s.Logfile)
									logfile.Close()
								}
								logfile = file
							}
						}
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (*loggingJob) Type() string {
	return loggingTypeName
}

func (job *loggingJob) Send(v interface{}) {
	values := []interface{}{v, nil}
	for _, v := range values {
		select {
		case job.event <- v:
		case <-job.done:
			return
		}
	}
}

func (job *loggingJob) Stop() {
	job.cancel()
}

func (job *loggingJob) Done() <-chan struct{} {
	return job.done
}

type loggingService struct{}

func (loggingService) Type() string {
	return loggingTypeName
}

func (loggingService) UnmarshalSettings(data []byte) (interface{}, error) {
	var settings loggingSettings
	if err := yaml.UnmarshalStrict(data, &settings.Logfile); err != nil {
		return nil, err
	}
	return settings, nil
}

func (loggingService) StartNewJob(name string) Job {
	job := loggingJob{
		name:  name,
		event: make(chan interface{}),
		done:  make(chan struct{}),
	}
	job.start()
	return &job
}

func init() {
	services.Add(loggingService{})
}
