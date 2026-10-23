package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port                   string
	ShutdownTimeout        time.Duration
	PlatformDatabaseURL    string
	BootstrapAdminUsername string
	BootstrapAdminPassword string
	SecureCookie           bool
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
	secureCookie := os.Getenv("APP_ENV") == "production"
	if value := os.Getenv("COOKIE_SECURE"); value != "" {
		configured, parseErr := strconv.ParseBool(value)
		if parseErr != nil {
			return Config{}, fmt.Errorf("COOKIE_SECURE must be true or false")
		}
		secureCookie = secureCookie || configured
	}
	return Config{
		Port:                   port,
		ShutdownTimeout:        timeout,
		PlatformDatabaseURL:    os.Getenv("PLATFORM_DATABASE_URL"),
		BootstrapAdminUsername: os.Getenv("BOOTSTRAP_ADMIN_USERNAME"),
		BootstrapAdminPassword: os.Getenv("BOOTSTRAP_ADMIN_PASSWORD"),
		SecureCookie:           secureCookie,
	}, nil
}

func (c Config) Address() string { return net.JoinHostPort("", c.Port) }
