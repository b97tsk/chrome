package service

import (
	"os"
)

type String string

func (s String) String() string {
	return string(s)
}

func (s *String) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var raw string
	if err := unmarshal(&raw); err != nil {
		return err
	}

	*s = String(os.ExpandEnv(raw))

	return nil
}
