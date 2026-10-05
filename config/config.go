// Package config provides helpers for parsing configuration values from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// PositiveDurationFromEnv parses a positive time.Duration from key, returning def if unset.
func PositiveDurationFromEnv(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be a positive Go duration (for example 30m): %w", key, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration (for example 30m), got %q", key, v)
	}
	return d, nil
}

// BoolFromEnv parses a boolean value from key, returning def if unset.
func BoolFromEnv(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean (1/true/0/false), got %q", key, v)
	}
	return b, nil
}
