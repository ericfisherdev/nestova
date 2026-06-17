// Package config loads runtime configuration from the environment.
package config

import (
	"os"
	"strings"
)

// Config holds the runtime configuration for the server.
type Config struct {
	// Addr is the TCP address the HTTP server listens on (e.g. ":8080").
	Addr string
	// Env is the deployment environment ("dev", "test", "prod").
	Env string
}

// Load reads configuration from the environment, applying sensible defaults so
// the server is runnable out of the box without any environment setup.
func Load() Config {
	// PORT is conventionally a bare port number; tolerate a leading colon
	// (e.g. PORT=":8080") so it does not produce a malformed "::8080" address.
	port := strings.TrimPrefix(getenv("PORT", "8080"), ":")
	return Config{
		Addr: ":" + port,
		Env:  getenv("APP_ENV", "dev"),
	}
}

// getenv returns the value of the environment variable named key, or fallback
// when the variable is unset or empty.
func getenv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
