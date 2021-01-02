package service

import (
	"errors"
	"strings"
)

type logLevel int32

const (
	logLevelError logLevel = iota - 2
	logLevelWarn
	logLevelInfo
	logLevelDebug
	logLevelTrace
)

func (l logLevel) String() string {
	switch l {
	case logLevelError:
		return "ERROR"
	case logLevelWarn:
		return "WARN"
	case logLevelInfo:
		return "INFO"
	case logLevelDebug:
		return "DEBUG"
	case logLevelTrace:
		return "TRACE"
	}

	panic("unknown log level")
}

func (l *logLevel) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string

	if err := unmarshal(&s); err != nil {
		return err
	}

	switch strings.ToUpper(s) {
	case "ERROR":
		*l = logLevelError
	case "WARN":
		*l = logLevelWarn
	case "INFO":
		*l = logLevelInfo
	case "DEBUG":
		*l = logLevelDebug
	case "TRACE":
		*l = logLevelTrace
	default:
		return errors.New("unknown log level")
	}

	return nil
}
