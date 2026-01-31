package config

import (
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// ServerConfig 服务端配置
type ServerConfig struct {
	Listen   string `yaml:"listen"`
	CertFile string `yaml:"cert"`
	KeyFile  string `yaml:"key"`
	Token    string `yaml:"token"`
	WSPath   string `yaml:"ws_path"`
	
	// 性能调优
	MaxStreamsPerConn int           `yaml:"max_streams_per_conn"`
	ReadTimeout       time.Duration `yaml:"read_timeout"`
	WriteTimeout      time.Duration `yaml:"write_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	
	// 日志
	LogLevel string `yaml:"log_level"`
}

func DefaultServerConfig() *ServerConfig {
	return &ServerConfig{
		Listen:            ":443",
		CertFile:          "cert.pem",
		KeyFile:           "key.pem",
		WSPath:            "/tunnel",
		MaxStreamsPerConn: 1000,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
		LogLevel:          "info",
	}
}

// ClientConfig 客户端配置
type ClientConfig struct {
	Server   string `yaml:"server"`
	Token    string `yaml:"token"`
	ClientID string `yaml:"client_id"`
	
	// SOCKS5
	Socks5Listen string `yaml:"socks5_listen"`
	Socks5Auth   string `yaml:"socks5_auth"` // user:pass
	
	// 连接池
	NumConnections int           `yaml:"num_connections"`
	WriteTimeout   time.Duration `yaml:"write_timeout"`
	ReadTimeout    time.Duration `yaml:"read_timeout"`
	
	// TLS
	Insecure bool `yaml:"insecure"`
	
	// ECH
	EnableECH bool   `yaml:"enable_ech"`
	ECHDomain string `yaml:"ech_domain"`
	ECHDns    string `yaml:"ech_dns"`
	
	// IP 策略
	IPStrategy string `yaml:"ip_strategy"` // 4, 6, 4,6, 6,4
	
	// 日志
	LogLevel string `yaml:"log_level"`
}

func DefaultClientConfig() *ClientConfig {
	return &ClientConfig{
		Socks5Listen:   ":1080",
		NumConnections: 3,
		WriteTimeout:   10 * time.Second,
		ReadTimeout:    60 * time.Second,
		EnableECH:      true,
		ECHDomain:      "cloudflare-ech.com",
		ECHDns:         "https://doh.pub/dns-query",
		IPStrategy:     "",
		LogLevel:       "info",
	}
}

func LoadServerConfig(path string) (*ServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return DefaultServerConfig(), nil
	}
	
	cfg := DefaultServerConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func LoadClientConfig(path string) (*ClientConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return DefaultClientConfig(), nil
	}
	
	cfg := DefaultClientConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
