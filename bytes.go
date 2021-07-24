package chrome

import (
	"strconv"

	"gopkg.in/yaml.v3"
)

// Bytes is a helper type for unmarshaling an int64 from YAML.
// When unmarshaling, Bytes accepts a binary prefix, for example, 1K = 1024.
type Bytes int64

func (b *Bytes) UnmarshalYAML(v *yaml.Node) error {
	var s string
	if err := v.Decode(&s); err != nil {
		return err
	}

	if s == "" {
		*b = 0
		return nil
	}

	n := 0

	switch s[len(s)-1] {
	case 'K':
		n = 10
	case 'M':
		n = 20
	case 'G':
		n = 30
	case 'T':
		n = 40
	}

	if n > 0 {
		s = s[:len(s)-1]
	}

	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}

	*b = Bytes(i << n)

	return nil
}
