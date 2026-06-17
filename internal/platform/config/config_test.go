package config_test

import (
	"testing"

	"github.com/ericfisherdev/nestova/internal/platform/config"
)

// TestLoad is the reference example for the project's test conventions:
// table-driven, black-box (package config_test), exact comparisons, and
// environment isolation via t.Setenv.
func TestLoad(t *testing.T) {
	tests := []struct {
		name     string
		port     string
		appEnv   string
		wantAddr string
		wantEnv  string
	}{
		{
			name:     "falls back to defaults when empty",
			port:     "",
			appEnv:   "",
			wantAddr: ":8080",
			wantEnv:  "dev",
		},
		{
			name:     "PORT and APP_ENV override defaults",
			port:     "9090",
			appEnv:   "prod",
			wantAddr: ":9090",
			wantEnv:  "prod",
		},
		{
			name:     "colon-prefixed PORT is normalized",
			port:     ":3000",
			appEnv:   "",
			wantAddr: ":3000",
			wantEnv:  "dev",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// t.Setenv isolates each case from the ambient environment and
			// auto-restores afterwards (so it cannot be combined with t.Parallel).
			t.Setenv("PORT", tt.port)
			t.Setenv("APP_ENV", tt.appEnv)

			got := config.Load()

			if got.Addr != tt.wantAddr {
				t.Errorf("Load().Addr = %q, want %q", got.Addr, tt.wantAddr)
			}
			if got.Env != tt.wantEnv {
				t.Errorf("Load().Env = %q, want %q", got.Env, tt.wantEnv)
			}
		})
	}
}
