// Package config contains process configuration primitives that are safe to
// keep in snapshots and configuration files.  Secrets are deliberately
// resolved through a server-side CredentialStore instead of being fields on a
// profile or an HTTP request DTO.
package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ErrCredentialNotFound is returned when a profile references a credential
// that has not been provisioned in the server-side store.
var ErrCredentialNotFound = errors.New("config: credential not found")

// ErrCredentialRefInvalid indicates that a credential reference is malformed.
// References are identifiers, not secret values, and are intentionally kept
// small so they can safely appear in profile metadata.
var ErrCredentialRefInvalid = errors.New("config: invalid credential reference")

// CredentialStore resolves a credential reference at request time.  The
// returned string is an in-process secret and callers must not put it into
// logs, snapshots, errors, or JSON.  Implementations should treat ctx
// cancellation as a failed lookup and should not perform network I/O.
type CredentialStore interface {
	Resolve(ctx context.Context, reference string) (string, error)
}

// SecretStore is a vocabulary alias for CredentialStore.  Both names refer
// to the same server-side, request-time resolver and neither stores a secret
// in profile DTOs.
type SecretStore = CredentialStore

// MemoryCredentialStore is a small process-local store for the MVP and local
// tests.  It is not intended to be durable storage.  Its JSON representation
// deliberately contains only references and never values, which makes it safe
// to include a configured-state diagnostic in a snapshot.
type MemoryCredentialStore struct {
	mu     sync.RWMutex
	values map[string]string
}

// NewMemoryCredentialStore constructs an empty in-memory credential store.
func NewMemoryCredentialStore() *MemoryCredentialStore {
	return &MemoryCredentialStore{values: make(map[string]string)}
}

// Put provisions value under reference.  The value is never returned in an
// error.  Empty values are rejected because injecting an empty credential is
// almost always a configuration mistake and makes auth failures ambiguous.
func (s *MemoryCredentialStore) Put(reference, value string) error {
	if err := ValidateCredentialReference(reference); err != nil {
		return err
	}
	if value == "" {
		return errors.New("config: credential value cannot be empty")
	}
	if strings.ContainsAny(value, "\r\n") {
		return errors.New("config: credential value contains CRLF")
	}
	if s == nil {
		return errors.New("config: nil credential store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = make(map[string]string)
	}
	s.values[reference] = value
	return nil
}

// Set is an alias for Put for callers that use configuration terminology.
func (s *MemoryCredentialStore) Set(reference, value string) error {
	return s.Put(reference, value)
}

// Delete removes a reference.  Deleting an unknown reference is harmless.
func (s *MemoryCredentialStore) Delete(reference string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, reference)
}

// Has reports whether a reference is provisioned without revealing its value.
func (s *MemoryCredentialStore) Has(reference string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.values[reference]
	return ok
}

// Resolve implements CredentialStore.  Neither errors nor returned metadata
// include the secret value.
func (s *MemoryCredentialStore) Resolve(ctx context.Context, reference string) (string, error) {
	if err := ValidateCredentialReference(reference); err != nil {
		return "", err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s == nil {
		return "", errors.New("config: nil credential store")
	}
	s.mu.RLock()
	value, ok := s.values[reference]
	s.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrCredentialNotFound, reference)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return value, nil
}

// ValidateCredentialReference validates the non-secret identifier stored in a
// profile.  It rejects whitespace/control characters and path-like values so
// an accidental secret or filesystem path is less likely to be persisted as a
// reference.  The identifier grammar intentionally allows slashes and dots
// for namespaced secret managers, while forbidding traversal and CRLF.
func ValidateCredentialReference(reference string) error {
	if reference == "" || len(reference) > 256 {
		return ErrCredentialRefInvalid
	}
	if strings.TrimSpace(reference) != reference || strings.ContainsAny(reference, "\r\n") {
		return ErrCredentialRefInvalid
	}
	for _, r := range reference {
		if r < 0x20 || r == 0x7f {
			return ErrCredentialRefInvalid
		}
	}
	if reference == "." || reference == ".." || strings.Contains(reference, "../") || strings.Contains(reference, "..\\") || strings.ContainsRune(reference, '\\') {
		return ErrCredentialRefInvalid
	}
	return nil
}

// CredentialState is a secret-free description suitable for profile metadata.
// A configured credential is represented only by its reference and scheme;
// Value is intentionally not present in this type.
type CredentialState struct {
	Reference  string `json:"credential_ref,omitempty"`
	Configured bool   `json:"configured"`
}

// MarshalJSON on MemoryCredentialStore emits only provisioned references.  It
// is intentionally not a way to retrieve values.
func (s *MemoryCredentialStore) MarshalJSON() ([]byte, error) {
	refs := make([]string, 0)
	if s != nil {
		s.mu.RLock()
		for ref := range s.values {
			refs = append(refs, ref)
		}
		s.mu.RUnlock()
	}
	sort.Strings(refs)
	return json.Marshal(struct {
		References []string `json:"references"`
	}{References: refs})
}

// String intentionally omits values because this type is often included in
// startup diagnostics.  Use Has or Resolve inside the request path instead.
func (s *MemoryCredentialStore) String() string {
	if s == nil {
		return "MemoryCredentialStore{nil}"
	}
	s.mu.RLock()
	count := len(s.values)
	s.mu.RUnlock()
	return fmt.Sprintf("MemoryCredentialStore{configured:%d}", count)
}

// GoString keeps %#v diagnostics secret-free as well.
func (s *MemoryCredentialStore) GoString() string { return s.String() }
