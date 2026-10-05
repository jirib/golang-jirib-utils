package config

import (
	"testing"
	"time"
)

func TestPositiveDurationFromEnv(t *testing.T) {
	t.Run("unset uses default", func(t *testing.T) {
		d, err := PositiveDurationFromEnv("MCPUTILS_TEST_UNSET_DUR", 5*time.Minute)
		if err != nil || d != 5*time.Minute {
			t.Fatalf("got %v, %v; want 5m, nil", d, err)
		}
	})
	t.Run("valid value overrides default", func(t *testing.T) {
		t.Setenv("MCPUTILS_TEST_DUR", "30s")
		d, err := PositiveDurationFromEnv("MCPUTILS_TEST_DUR", time.Minute)
		if err != nil || d != 30*time.Second {
			t.Fatalf("got %v, %v; want 30s, nil", d, err)
		}
	})
	t.Run("zero is rejected", func(t *testing.T) {
		t.Setenv("MCPUTILS_TEST_DUR_ZERO", "0s")
		if _, err := PositiveDurationFromEnv("MCPUTILS_TEST_DUR_ZERO", time.Minute); err == nil {
			t.Fatal("expected an error for a non-positive duration, got nil")
		}
	})
	t.Run("garbage is rejected", func(t *testing.T) {
		t.Setenv("MCPUTILS_TEST_DUR_BAD", "not-a-duration")
		if _, err := PositiveDurationFromEnv("MCPUTILS_TEST_DUR_BAD", time.Minute); err == nil {
			t.Fatal("expected an error for an unparseable duration, got nil")
		}
	})
}

func TestBoolFromEnv(t *testing.T) {
	t.Run("unset uses default", func(t *testing.T) {
		b, err := BoolFromEnv("MCPUTILS_TEST_UNSET_BOOL", true)
		if err != nil || !b {
			t.Fatalf("got %v, %v; want true, nil", b, err)
		}
	})
	t.Run("valid value overrides default", func(t *testing.T) {
		t.Setenv("MCPUTILS_TEST_BOOL", "true")
		b, err := BoolFromEnv("MCPUTILS_TEST_BOOL", false)
		if err != nil || !b {
			t.Fatalf("got %v, %v; want true, nil", b, err)
		}
	})
	t.Run("garbage is rejected", func(t *testing.T) {
		t.Setenv("MCPUTILS_TEST_BOOL_BAD", "not-a-bool")
		if _, err := BoolFromEnv("MCPUTILS_TEST_BOOL_BAD", false); err == nil {
			t.Fatal("expected an error for an unparseable bool, got nil")
		}
	})
}
