package chrome

import (
	"os"

	"gopkg.in/yaml.v3"
)

// EnvString is a helper type for unmarshaling a string from YAML.
// When unmarshaling, it calls os.ExpandEnv on the original string and
// stores the result.
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
