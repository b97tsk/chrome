package main

import (
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v2"
)

type Config struct {
	Logfile string `yaml:"logging"`
	Aliases []interface{}
	Jobs    map[string]interface{} `yaml:",inline"`
}

func (c *Config) Unmarshal(r io.Reader) error {
	dec := yaml.NewDecoder(r)
	dec.SetStrict(true)
	for {
		var d Config
		err := dec.Decode(&d)
		if err == nil {
			err = c.mergeWith(d)
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func (c *Config) mergeWith(d Config) error {
	if d.Logfile != "" {
		if c.Logfile != "" {
			return errors.New("duplicate logging")
		}
		c.Logfile = d.Logfile
	}
	if c.Jobs != nil {
		for name, data := range d.Jobs {
			if _, duplicated := c.Jobs[name]; duplicated {
				return fmt.Errorf("duplicate: %v", name)
			}
			c.Jobs[name] = data
		}
	} else {
		c.Jobs = d.Jobs
	}
	return nil
}
