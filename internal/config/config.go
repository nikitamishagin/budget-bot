package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

// LLMConfig holds settings required to talk to the local LLM inference server.
type LLMConfig struct {
	BaseURL        string `mapstructure:"base_url"`
	APIKeyFile     string `mapstructure:"api_key_file"`
	Model          string `mapstructure:"model"`
	TimeoutSeconds int    `mapstructure:"timeout_seconds"`
}

// LogConfig holds logging settings.
type LogConfig struct {
	Level string `mapstructure:"level"`
}

// Config is the root application configuration.
type Config struct {
	LLM LLMConfig `mapstructure:"llm"`
	Log LogConfig `mapstructure:"log"`
}

// Load reads the configuration from the given YAML file path and applies defaults.
func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")

	v.SetDefault("llm.base_url", "http://127.0.0.1:8080/v1")
	v.SetDefault("llm.api_key_file", "./api-key")
	v.SetDefault("llm.model", "config_qwen35-9b-nvfp4")
	v.SetDefault("llm.timeout_seconds", 120)
	v.SetDefault("log.level", "info")

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config file %q: %w", path, err)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Validate checks that required fields are set.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.LLM.BaseURL) == "" {
		return fmt.Errorf("llm.base_url must not be empty")
	}
	if strings.TrimSpace(c.LLM.Model) == "" {
		return fmt.Errorf("llm.model must not be empty")
	}
	if c.LLM.TimeoutSeconds <= 0 {
		return fmt.Errorf("llm.timeout_seconds must be positive")
	}
	return nil
}

// APIKey reads and returns the trimmed API key from LLM.APIKeyFile.
func (c *Config) APIKey() (string, error) {
	if strings.TrimSpace(c.LLM.APIKeyFile) == "" {
		return "", nil
	}
	data, err := os.ReadFile(c.LLM.APIKeyFile)
	if err != nil {
		return "", fmt.Errorf("read api key file %q: %w", c.LLM.APIKeyFile, err)
	}
	return strings.TrimSpace(string(data)), nil
}
