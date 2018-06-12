package main

import (
	"io"
	"log"
	"os"

	"gopkg.in/yaml.v2"
)

type loggingOptions struct {
	Logfile string
}

type loggingService struct{}

func (loggingService) Run(ctx ServiceCtx) {
	var options loggingOptions
	var logfile *os.File
	defer func() {
		if logfile != nil {
			log.SetOutput(os.Stderr)
			logfile.Close()
			// log.Printf("[logging] closed %v\n", options.Logfile)
		}
	}()

	for {
		select {
		case data := <-ctx.Events:
			if new, ok := data.(loggingOptions); ok {
				old := options
				options = new
				if new.Logfile != old.Logfile {
					if new.Logfile == "" {
						if logfile != nil {
							log.SetOutput(os.Stderr)
							logfile.Close()
							logfile = nil
							// log.Printf("[logging] closed %v\n", old.Logfile)
						}
					} else {
						// log.Printf("[logging] opening %v\n", new.Logfile)
						name := os.ExpandEnv(new.Logfile)
						file, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE, 0644)
						if err != nil {
							log.Printf("[logging] %v", err)
						} else {
							log.SetOutput(io.MultiWriter(file, os.Stderr))
							if logfile != nil {
								logfile.Close()
								// log.Printf("[logging] closed %v\n", old.Logfile)
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

func (loggingService) UnmarshalOptions(text []byte) (interface{}, error) {
	var options loggingOptions
	if err := yaml.UnmarshalStrict(text, &options.Logfile); err != nil {
		return nil, err
	}
	return options, nil
}

func init() {
	services.Add("logging", loggingService{})
}
