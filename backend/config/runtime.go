package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"unicode"
)

// RuntimeConfig is the deliberately small process configuration used by the
// local launcher. APIKey is read only by the server process and is never
// included in diagnostics, workspace DTOs, or error messages.
//
// The checked-in example and the local file use exactly these two fields:
// base_url and api_key.
type RuntimeConfig struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

// DefaultRuntimeConfig points at the repository's local mock upstream. It is
// used by the one-command development launcher when it creates config.local.
func DefaultRuntimeConfig() RuntimeConfig {
	return RuntimeConfig{BaseURL: "http://127.0.0.1:19091"}
}

// LoadRuntimeConfig reads and validates a local JSON config file. Unknown
// fields are rejected so a typo cannot silently change startup semantics.
// Files containing a group/world permission bit are rejected because the file
// may contain APIKey. The key itself is never included in returned errors.
func LoadRuntimeConfig(path string) (RuntimeConfig, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return RuntimeConfig{}, errors.New("config: runtime config path is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("config: cannot stat runtime config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return RuntimeConfig{}, errors.New("config: runtime config must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return RuntimeConfig{}, errors.New("config: runtime config must not be readable by group or others")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("config: cannot read runtime config: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var cfg RuntimeConfig
	if err := decoder.Decode(&cfg); err != nil {
		return RuntimeConfig{}, errors.New("config: runtime config is not valid JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return RuntimeConfig{}, errors.New("config: runtime config contains trailing JSON")
	}
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	if err := cfg.Validate(); err != nil {
		return RuntimeConfig{}, err
	}
	return cfg, nil
}

// Validate checks the non-secret shape of RuntimeConfig. The gateway applies
// the runtime loopback policy separately; this method only rejects malformed
// URL syntax and unsafe URL components.
func (c RuntimeConfig) Validate() error {
	if c.BaseURL == "" {
		return errors.New("config: base_url is required")
	}
	if strings.ContainsAny(c.BaseURL, "\r\n") {
		return errors.New("config: base_url contains CRLF")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return errors.New("config: base_url must be an absolute http(s) URL")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("config: base_url must not contain userinfo, query, or fragment")
	}
	if strings.ContainsAny(u.Host, "\r\n") {
		return errors.New("config: base_url host contains CRLF")
	}
	if strings.TrimSpace(c.APIKey) != c.APIKey {
		return errors.New("config: api_key must not have surrounding whitespace")
	}
	if strings.ContainsAny(c.APIKey, "\r\n") {
		return errors.New("config: api_key contains CRLF")
	}
	for _, r := range c.APIKey {
		if unicode.IsControl(r) {
			return errors.New("config: api_key contains a control character")
		}
	}
	return nil
}

// Summary is safe to use in startup logs. It intentionally contains no APIKey.
func (c RuntimeConfig) Summary() string {
	return fmt.Sprintf("RuntimeConfig{base_url:%q api_key_configured:%t}", c.BaseURL, c.APIKey != "")
}

func (c RuntimeConfig) String() string { return c.Summary() }
