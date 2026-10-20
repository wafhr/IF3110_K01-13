package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port            string
	ShutdownTimeout time.Duration
}

// Load reads configuration from process environment variables.
func Load() (Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return Config{}, fmt.Errorf("PORT must be an integer between 1 and 65535")
	}
	timeout := 10 * time.Second
	if value := os.Getenv("SHUTDOWN_TIMEOUT"); value != "" {
		timeout, err = time.ParseDuration(value)
		if err != nil || timeout <= 0 {
			return Config{}, fmt.Errorf("SHUTDOWN_TIMEOUT must be a positive duration, for example 10s")
		}
	}
	return Config{Port: port, ShutdownTimeout: timeout}, nil
}

func (c Config) Address() string { return net.JoinHostPort("", c.Port) }
