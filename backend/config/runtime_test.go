package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRuntimeConfig(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadRuntimeConfig(t *testing.T) {
	path := writeRuntimeConfig(t, `{"base_url":"http://127.0.0.1:19091","api_key":"synthetic-local-key"}`, 0o600)
	got, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseURL != "http://127.0.0.1:19091" || got.APIKey != "synthetic-local-key" {
		t.Fatalf("config = %+v", got)
	}
	if strings.Contains(got.Summary(), got.APIKey) {
		t.Fatal("runtime summary contains api key")
	}
}

func TestLoadRuntimeConfigRejectsUnsafeFileAndUnknownFields(t *testing.T) {
	if _, err := LoadRuntimeConfig(writeRuntimeConfig(t, `{"base_url":"http://127.0.0.1:19091","api_key":"x"}`, 0o644)); err == nil {
		t.Fatal("world-readable config accepted")
	}
	if _, err := LoadRuntimeConfig(writeRuntimeConfig(t, `{"base_url":"http://127.0.0.1:19091","api_key":"x","extra":true}`, 0o600)); err == nil {
		t.Fatal("unknown config field accepted")
	}
}

func TestRuntimeConfigValidation(t *testing.T) {
	for _, cfg := range []RuntimeConfig{
		{BaseURL: "https://example.com?api_key=secret"},
		{BaseURL: "http://example.com/user", APIKey: "bad\nvalue"},
		{BaseURL: "ftp://127.0.0.1:1"},
		{BaseURL: "http://127.0.0.1:19091", APIKey: " leading"},
	} {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("unsafe config accepted: %+v", cfg)
		}
	}
	if err := DefaultRuntimeConfig().Validate(); err != nil {
		t.Fatal(err)
	}
}
