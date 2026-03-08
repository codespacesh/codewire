package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// CodewireConfig represents the codewire.yaml schema.
type CodewireConfig struct {
	Template string            `yaml:"template"`
	Install  string            `yaml:"install"`
	Startup  string            `yaml:"startup"`
	Secrets  string            `yaml:"secrets"`
	Env      map[string]string `yaml:"env"`
	Ports    []PortConfig      `yaml:"ports"`
	CPU      int               `yaml:"cpu"`
	Memory   int               `yaml:"memory"`
	Disk     int               `yaml:"disk"`
	Agent    string            `yaml:"agent"`
}

// PortConfig represents a port in codewire.yaml.
type PortConfig struct {
	Port  int    `yaml:"port"`
	Label string `yaml:"label"`
}

// LoadCodewireConfig reads and parses a codewire.yaml file.
func LoadCodewireConfig(path string) (*CodewireConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var cfg CodewireConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	return &cfg, nil
}
