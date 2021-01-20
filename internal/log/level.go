package log

import (
	"strings"

	"gopkg.in/yaml.v3"
)

type Level int32

const (
	LevelNone Level = iota - 3
	LevelError
	LevelWarn
	LevelInfo
	LevelDebug
	LevelTrace
)

// Default log level is LevelInfo, which must be zero.
const _, _ = uint32(LevelInfo), uint32(-LevelInfo)

func (lv Level) String() string {
	switch lv {
	case LevelNone:
		return "NONE"
	case LevelError:
		return "ERROR"
	case LevelWarn:
		return "WARN"
	case LevelInfo:
		return "INFO"
	case LevelDebug:
		return "DEBUG"
	case LevelTrace:
		return "TRACE"
	}

	panic("unknown log level")
}

func (lv *Level) UnmarshalYAML(v *yaml.Node) error {
	var s string

	if err := v.Decode(&s); err != nil {
		return err
	}

	switch {
	case strings.EqualFold(s, "NONE"):
		*lv = LevelNone
	case strings.EqualFold(s, "ERROR"):
		*lv = LevelError
	case strings.EqualFold(s, "WARN"):
		*lv = LevelWarn
	case strings.EqualFold(s, "INFO"):
		*lv = LevelInfo
	case strings.EqualFold(s, "DEBUG"):
		*lv = LevelDebug
	case strings.EqualFold(s, "TRACE"):
		*lv = LevelTrace
	default:
		return UnknownLevelError(s)
	}

	return nil
}

type UnknownLevelError string

func (e UnknownLevelError) Error() string {
	return "unknown log level: " + string(e)
}
