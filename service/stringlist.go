package service

import (
	"errors"

	"gopkg.in/yaml.v3"
)

type StringList []string

func (s *StringList) UnmarshalYAML(v *yaml.Node) error {
	var slice []string
	if err := v.Decode(&slice); err == nil {
		*s = slice
		return nil
	}

	var raw string
	if err := v.Decode(&raw); err == nil {
		*s = []string{raw}
		return nil
	}

	return errors.New("invalid string list")
}
