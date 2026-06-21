package httpserver

import (
	"strconv"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/platform/config"
)

func TestHSTSHeaderValue(t *testing.T) {
	defaultSeconds := "max-age=" + strconv.FormatInt(int64(config.DefaultHSTSMaxAge.Seconds()), 10)

	cases := []struct {
		name string
		cfg  config.HSTSConfig
		want string
	}{
		{"disabled yields no header", config.HSTSConfig{Enabled: false, MaxAge: time.Hour, MaxAgeSet: true}, ""},
		{"max-age only", config.HSTSConfig{Enabled: true, MaxAge: 24 * time.Hour, MaxAgeSet: true}, "max-age=86400"},
		{
			"includeSubDomains without preload",
			config.HSTSConfig{Enabled: true, MaxAge: 24 * time.Hour, MaxAgeSet: true, IncludeSubdomains: true},
			"max-age=86400; includeSubDomains",
		},
		{
			"all directives",
			config.HSTSConfig{Enabled: true, MaxAge: 24 * time.Hour, MaxAgeSet: true, IncludeSubdomains: true, Preload: true},
			"max-age=86400; includeSubDomains; preload",
		},
		{"unset max-age uses the built-in default", config.HSTSConfig{Enabled: true, MaxAgeSet: false}, defaultSeconds},
		{"explicit zero max-age clears HSTS", config.HSTSConfig{Enabled: true, MaxAge: 0, MaxAgeSet: true}, "max-age=0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hstsHeaderValue(tc.cfg); got != tc.want {
				t.Errorf("hstsHeaderValue() = %q, want %q", got, tc.want)
			}
		})
	}
}
