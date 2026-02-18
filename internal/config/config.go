package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	API      APIConfig      `yaml:"api"`
	Storage  StorageConfig  `yaml:"storage"`
	FFmpeg   FFmpegConfig   `yaml:"ffmpeg"`
	Telegram TelegramConfig `yaml:"telegram"`
}

type APIConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type StorageConfig struct {
	DSN string `yaml:"dsn"`
}

type FFmpegConfig struct {
	Path         string `yaml:"path"`
	ProbePath    string `yaml:"probe_path"`
	MaxFlows     int    `yaml:"max_flows"`
	HealthTimeMs int    `yaml:"health_timeout_ms"` // failover health threshold
}

type TelegramConfig struct {
	Enabled  bool   `yaml:"enabled"`
	BotToken string `yaml:"bot_token"`
	ChatID   string `yaml:"chat_id"`
}

func Load(path string) (*Config, error) {
	cfg := Defaults()
	data, err := os.ReadFile(path)
	if err != nil {
		applyEnv(cfg)
		return cfg, nil
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	applyDefaults(cfg)
	applyEnv(cfg)
	return cfg, nil
}

func Defaults() *Config {
	return &Config{
		API:     APIConfig{Host: "0.0.0.0", Port: 8080},
		Storage: StorageConfig{DSN: "sqlite:./mroute.db"},
		FFmpeg: FFmpegConfig{
			Path:         "ffmpeg",
			ProbePath:    "ffprobe",
			MaxFlows:     20,
			HealthTimeMs: 500,
		},
	}
}

func applyDefaults(c *Config) {
	if c.API.Host == "" {
		c.API.Host = "0.0.0.0"
	}
	if c.API.Port == 0 {
		c.API.Port = 8080
	}
	if c.Storage.DSN == "" {
		c.Storage.DSN = "sqlite:./mroute.db"
	}
	if c.FFmpeg.Path == "" {
		c.FFmpeg.Path = "ffmpeg"
	}
	if c.FFmpeg.ProbePath == "" {
		c.FFmpeg.ProbePath = "ffprobe"
	}
	if c.FFmpeg.MaxFlows == 0 {
		c.FFmpeg.MaxFlows = 20
	}
	if c.FFmpeg.HealthTimeMs == 0 {
		c.FFmpeg.HealthTimeMs = 500
	}
}

func applyEnv(c *Config) {
	if v := os.Getenv("TELEGRAM_BOT_TOKEN"); v != "" {
		c.Telegram.BotToken = v
		c.Telegram.Enabled = true
	}
	if v := os.Getenv("TELEGRAM_CHAT_ID"); v != "" {
		c.Telegram.ChatID = v
	}
	if v := os.Getenv("MROUTE_PORT"); v != "" {
		fmt.Sscanf(v, "%d", &c.API.Port)
	}
}
