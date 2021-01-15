package service

import "errors"

type StringList []string

func (s *StringList) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var slice []string
	if err := unmarshal(&slice); err == nil {
		*s = slice
		return nil
	}

	var raw string
	if err := unmarshal(&raw); err == nil {
		*s = []string{raw}
		return nil
	}

	return errors.New("invalid string list")
}
