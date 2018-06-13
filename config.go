package main

import (
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v2"
)

type Config struct {
	Logfile string `yaml:"logging"`
	Proxies map[string]ProxyList
	Jobs    map[string]map[string]interface{} `yaml:",inline"`
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
	if c.Proxies != nil {
		for name, proxies := range d.Proxies {
			if _, duplicated := c.Proxies[name]; duplicated {
				return fmt.Errorf("duplicate proxy: %v", name)
			}
			c.Proxies[name] = proxies
		}
	} else {
		c.Proxies = d.Proxies
	}
	if c.Jobs != nil {
		for service, jobs := range d.Jobs {
			jobsMerged := c.Jobs[service]
			if jobsMerged == nil {
				c.Jobs[service] = jobs
				continue
			}
			for name, data := range jobs {
				if _, duplicated := jobsMerged[name]; duplicated {
					return fmt.Errorf("duplicate %v service: %v", service, name)
				}
				jobsMerged[name] = data
			}
		}
	} else {
		c.Jobs = d.Jobs
	}
	return nil
}
