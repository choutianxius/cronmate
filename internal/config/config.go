package config

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/spf13/viper"
)

type Config struct {
	DataDir    string      `mapstructure:"data_dir" yaml:"data_dir"`
	PromptsDir string      `mapstructure:"prompts_dir" yaml:"prompts_dir"`
	LogLevel   string      `mapstructure:"log_level" yaml:"log_level"`
	TimeZone   string      `mapstructure:"time_zone" yaml:"time_zone"`
	Agent      AgentConfig `mapstructure:"agent" yaml:"agent"`

	// TODO: Add full config data structure when needed
}

type AgentConfig struct {
	Provider string `mapstructure:"provider" yaml:"provider"`
	Model    string `mapstructure:"model" yaml:"model"`
	BaseURL  string `mapstructure:"base_url" yaml:"base_url"`
}

var (
	cfg  *Config
	v    *viper.Viper = viper.New()
	mu   sync.RWMutex
	once sync.Once
)

func Init() error {
	// TODO: Initialize vars

	// Load from the config file, and fall back to built-in defaults
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configPath := filepath.Join(homeDir, ".config", "cronmate", "config.yaml")
	v.AddConfigPath(configPath)
	err = v.ReadInConfig()
	if err != nil {
		return err
	}

	// 0. global variable initialization with built-in defaults

	// 1. parse config file, create if none

	panic("Not implemented")
}

func Get(key string) (any, error) {
	panic("Not implemented")
}

func Set(key string, value any) error {
	panic("Not implemented")
}

// User can edit the config file directly so we expose Reload to synchronize changes
func Reload() error {
	panic("Not implemented")
}
