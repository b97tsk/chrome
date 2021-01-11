package service

import (
	"os"
)

type EnvString string

func (s EnvString) String() string {
	return string(s)
}

func (s *EnvString) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var raw string
	if err := unmarshal(&raw); err != nil {
		return err
	}

	*s = EnvString(os.ExpandEnv(raw))

	return nil
}
