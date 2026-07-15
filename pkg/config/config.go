package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Config struct {
	Workers     int      `json:"workers"`
	Transport   string   `json:"transport"`
	Bandwidth   int64    `json:"bandwidth"`
	Compression bool     `json:"compression"`
	Checksum    bool     `json:"checksum"`
	Resume      bool     `json:"resume"`
	Verbose     bool     `json:"verbose"`
	Quiet       bool     `json:"quiet"`
	Excludes    []string `json:"excludes"`
	Includes    []string `json:"includes"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, err
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	return &config, nil
}

func SaveConfig(path string, config *Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}

func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".gosync.json"
	}
	return filepath.Join(home, ".gosync.json")
}

func MergeConfig(base, override *Config) *Config {
	result := *base

	if override.Workers > 0 {
		result.Workers = override.Workers
	}
	if override.Transport != "" {
		result.Transport = override.Transport
	}
	if override.Bandwidth > 0 {
		result.Bandwidth = override.Bandwidth
	}
	if override.Compression {
		result.Compression = override.Compression
	}
	if override.Checksum {
		result.Checksum = override.Checksum
	}
	if override.Resume {
		result.Resume = override.Resume
	}
	if override.Verbose {
		result.Verbose = override.Verbose
	}
	if override.Quiet {
		result.Quiet = override.Quiet
	}
	if len(override.Excludes) > 0 {
		result.Excludes = override.Excludes
	}
	if len(override.Includes) > 0 {
		result.Includes = override.Includes
	}

	return &result
}
