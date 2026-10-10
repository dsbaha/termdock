// Package config loads TermDock configuration from environment variables.
package config

import (
	"os"
	"strconv"
)

// Config holds all runtime configuration for TermDock.
type Config struct {
	Addr           string // listen address, e.g. ":8080"
	DataDir        string // directory holding termdock.db
	RegOpen        bool   // whether /api/auth/register is enabled
	DefaultImage   string // default container image for new sessions
	DefaultIdleMin int    // default idle timeout in minutes (0 = never)
	SecureCookie   bool   // set session cookie Secure flag (enable behind TLS)
}

// Load reads configuration from the environment.
func Load() Config {
	return Config{
		Addr:           envOr("TERMDOCK_ADDR", ":8080"),
		DataDir:        envOr("TERMDOCK_DATA", "./data"),
		RegOpen:        envBool("TERMDOCK_REG_OPEN", true),
		DefaultImage:   envOr("TERMDOCK_DEFAULT_IMAGE", "ubuntu:24.04"),
		DefaultIdleMin: envInt("TERMDOCK_IDLE_DEFAULT", 60),
		SecureCookie:   envBool("TERMDOCK_SECURE_COOKIE", false),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
