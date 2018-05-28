package main

import (
	"io"
	"log"
	"os"

	"gopkg.in/yaml.v2"
)

type loggingSettings struct {
	Logfile string
}

type loggingService struct{}

func (loggingService) Run(ctx ServiceCtx) {
	var settings loggingSettings
	var logfile *os.File
	defer func() {
		if logfile != nil {
			log.SetOutput(os.Stderr)
			logfile.Close()
			// log.Printf("[logging] closed %v\n", settings.Logfile)
		}
	}()

	for {
		select {
		case data := <-ctx.Events:
			if data == nil {
				continue
			}
			var s loggingSettings
			bytes, _ := yaml.Marshal(data)
			if err := yaml.UnmarshalStrict(bytes, &s.Logfile); err != nil {
				log.Printf("[logging] unmarshal: %v\n", err)
				continue
			}
			settings, s = s, settings
			if settings.Logfile != s.Logfile {
				if settings.Logfile == "" {
					if logfile != nil {
						log.SetOutput(os.Stderr)
						logfile.Close()
						logfile = nil
						// log.Printf("[logging] closed %v\n", s.Logfile)
					}
				} else {
					// log.Printf("[logging] opening %v\n", settings.Logfile)
					file, err := os.OpenFile(settings.Logfile, os.O_APPEND|os.O_CREATE, 0644)
					if err != nil {
						log.Printf("[logging] %v", err)
					} else {
						log.SetOutput(io.MultiWriter(file, os.Stderr))
						if logfile != nil {
							logfile.Close()
							// log.Printf("[logging] closed %v\n", s.Logfile)
						}
						logfile = file
					}
				}
			}
		case <-ctx.Done:
			return
		}
	}
}

func init() {
	services.Add("logging", loggingService{})
}
