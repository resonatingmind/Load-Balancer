// Package config loads and validates the load balancer configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// Duration wraps time.Duration so it can be written as "5s" or "250ms" in JSON.
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"5s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

type Config struct {
	ListenAddr string          `json:"listen_addr"`
	AdminAddr  string          `json:"admin_addr"` // empty disables the admin server
	Strategy   string          `json:"strategy"`   // "least_connections" or "round_robin"
	Backends   []BackendConfig `json:"backends"`
	Transport  TransportConfig `json:"transport"`
	Server     ServerConfig    `json:"server"`
}

type BackendConfig struct {
	URL string `json:"url"`
}

// TransportConfig controls the pooled connections from the LB to the backends.
type TransportConfig struct {
	DialTimeout           Duration `json:"dial_timeout"`
	MaxIdleConns          int      `json:"max_idle_conns"`
	MaxIdleConnsPerHost   int      `json:"max_idle_conns_per_host"`
	MaxConnsPerHost       int      `json:"max_conns_per_host"` // 0 means unlimited
	IdleConnTimeout       Duration `json:"idle_conn_timeout"`
	ResponseHeaderTimeout Duration `json:"response_header_timeout"`
}

// ServerConfig controls the client-facing HTTP server.
type ServerConfig struct {
	ReadHeaderTimeout Duration `json:"read_header_timeout"`
	ReadTimeout       Duration `json:"read_timeout"`
	WriteTimeout      Duration `json:"write_timeout"`
	IdleTimeout       Duration `json:"idle_timeout"`
	ShutdownTimeout   Duration `json:"shutdown_timeout"`
}

// Default returns a config with sensible production defaults. Values in the
// config file override these.
func Default() *Config {
	return &Config{
		ListenAddr: ":8080",
		AdminAddr:  ":9090",
		Strategy:   "least_connections",
		Transport: TransportConfig{
			DialTimeout:           Duration{2 * time.Second},
			MaxIdleConns:          1000,
			MaxIdleConnsPerHost:   256,
			IdleConnTimeout:       Duration{90 * time.Second},
			ResponseHeaderTimeout: Duration{10 * time.Second},
		},
		Server: ServerConfig{
			ReadHeaderTimeout: Duration{5 * time.Second},
			ReadTimeout:       Duration{30 * time.Second},
			WriteTimeout:      Duration{30 * time.Second},
			IdleTimeout:       Duration{120 * time.Second},
			ShutdownTimeout:   Duration{15 * time.Second},
		},
	}
}

// Load reads a JSON config file on top of the defaults and validates it.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := Default()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

func (c *Config) Validate() error {
	if c.ListenAddr == "" {
		return errors.New("listen_addr is required")
	}
	if len(c.Backends) == 0 {
		return errors.New("at least one backend is required")
	}
	for i, b := range c.Backends {
		if b.URL == "" {
			return fmt.Errorf("backends[%d].url is required", i)
		}
	}
	if c.Transport.MaxIdleConns < 0 || c.Transport.MaxIdleConnsPerHost < 0 || c.Transport.MaxConnsPerHost < 0 {
		return errors.New("transport connection limits must be >= 0")
	}
	return nil
}
