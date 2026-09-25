package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/viper"
)

type Config struct {
	DataDir    string      `mapstructure:"data_dir" yaml:"data_dir"`
	PromptsDir string      `mapstructure:"prompts_dir" yaml:"prompts_dir"`
	LogLevel   string      `mapstructure:"log_level" yaml:"log_level"`
	TimeZone   string      `mapstructure:"time_zone" yaml:"time_zone"`
	Agent      AgentConfig `mapstructure:"agent" yaml:"agent"`
}

type AgentConfig struct {
	Provider string `mapstructure:"provider" yaml:"provider"`
	Model    string `mapstructure:"model" yaml:"model"`
	BaseURL  string `mapstructure:"base_url" yaml:"base_url"`
}

// ErrNotInitialized is returned when the current config is requested before a
// successful load.
var ErrNotInitialized = errors.New("config is not initialized")

var (
	cfg  *Config
	v    *viper.Viper = viper.New()
	mu   sync.RWMutex
	once sync.Once
	raw  []byte
)

// Path returns the only configuration file location supported by cronmate.
func Path() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}

	return filepath.Join(homeDir, ".config", "cronmate", "config.yaml"), nil
}

// Load reads the configuration from disk and makes it the current config.
// The file is never created or modified by this package.
func Load() (Config, error) {
	mu.Lock()
	defer mu.Unlock()

	configPath, err := Path()
	if err != nil {
		return Config{}, err
	}

	once.Do(func() {
		v.SetConfigType("yaml")
	})
	v.SetConfigFile(configPath)

	contents, err := os.ReadFile(configPath)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", configPath, err)
	}
	if err := v.ReadConfig(bytes.NewReader(contents)); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", configPath, err)
	}

	var parsed Config
	if err := v.Unmarshal(&parsed); err != nil {
		if rollbackErr := v.ReadConfig(bytes.NewReader(raw)); rollbackErr != nil {
			return Config{}, fmt.Errorf(
				"parse config %q: %w (restore previous config: %v)",
				configPath,
				err,
				rollbackErr,
			)
		}
		return Config{}, fmt.Errorf("parse config %q: %w", configPath, err)
	}

	cfg = &parsed
	raw = append(raw[:0], contents...)

	return parsed, nil
}

// Init loads the configuration. It is kept as the initialization entry point
// used by the CLI.
func Init() error {
	_, err := Load()
	return err
}

// Current returns a snapshot of the last successfully loaded configuration.
func Current() (Config, error) {
	mu.RLock()
	defer mu.RUnlock()

	if cfg == nil {
		return Config{}, ErrNotInitialized
	}

	return *cfg, nil
}

// ReadRaw reads the config file without parsing or changing the current config.
func ReadRaw() ([]byte, error) {
	configPath, err := Path()
	if err != nil {
		return nil, err
	}

	contents, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", configPath, err)
	}

	return contents, nil
}

// Get returns a value from the last successfully loaded configuration using a
// dotted key such as "agent.provider".
func Get(key string) (any, error) {
	if key == "" {
		return nil, errors.New("config key cannot be empty")
	}

	mu.RLock()
	defer mu.RUnlock()

	if cfg == nil {
		return nil, ErrNotInitialized
	}

	for _, part := range strings.Split(key, ".") {
		if part == "" {
			return nil, fmt.Errorf("config key %q not found", key)
		}
	}
	if !v.IsSet(key) {
		return nil, fmt.Errorf("config key %q not found", key)
	}

	return cloneValue(v.Get(key)), nil
}

// Reload replaces the current config only after a complete, successful read.
func Reload() error {
	_, err := Load()
	return err
}

func cloneValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(value))
		for key, item := range value {
			cloned[key] = cloneValue(item)
		}
		return cloned
	case []any:
		cloned := make([]any, len(value))
		for index, item := range value {
			cloned[index] = cloneValue(item)
		}
		return cloned
	default:
		return value
	}
}
