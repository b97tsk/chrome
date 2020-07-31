package logging

import (
	"io"
	"log"
	"os"

	"github.com/b97tsk/chrome/service"
	"gopkg.in/yaml.v2"
)

type Options struct {
	Logfile service.String
}

type Service struct{}

func (Service) Name() string {
	return "logging"
}

func (Service) Run(ctx service.Context) {
	var options Options

	var logfile *os.File
	defer func() {
		if logfile != nil {
			log.SetOutput(os.Stderr)
			logfile.Close()
		}
	}()

	for {
		select {
		case opts := <-ctx.Opts:
			if new, ok := opts.(Options); ok {
				old := options
				options = new
				if new.Logfile != old.Logfile {
					if new.Logfile == "" {
						if logfile != nil {
							log.SetOutput(os.Stderr)
							logfile.Close()
							logfile = nil
						}
					} else {
						file, err := os.OpenFile(new.Logfile.String(), os.O_APPEND|os.O_CREATE, 0644)
						if err != nil {
							writeLog(err)
						} else {
							log.SetOutput(io.MultiWriter(file, os.Stderr))
							if logfile != nil {
								logfile.Close()
							}
							logfile = file
						}
					}
				}
			}
		case <-ctx.Done:
			return
		}
	}
}

func (Service) UnmarshalOptions(text []byte) (interface{}, error) {
	var opts Options
	if err := yaml.UnmarshalStrict(text, &opts.Logfile); err != nil {
		return nil, err
	}
	return opts, nil
}
