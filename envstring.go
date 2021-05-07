package chrome

import (
	"os"

	"gopkg.in/yaml.v3"
)

type EnvString string

func (s EnvString) String() string {
	return string(s)
}

func (s *EnvString) UnmarshalYAML(v *yaml.Node) error {
	var raw string
	if err := v.Decode(&raw); err != nil {
		return err
	}

	*s = EnvString(os.ExpandEnv(raw))

	return nil
}
