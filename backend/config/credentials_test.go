package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestMemoryCredentialStoreResolvesWithoutJSONSecret(t *testing.T) {
	store := NewMemoryCredentialStore()
	const secret = "sk-test-only-never-in-json"
	if err := store.Put("openai/local", secret); err != nil {
		t.Fatalf("put: %v", err)
	}
	if !store.Has("openai/local") {
		t.Fatal("provisioned credential not reported configured")
	}
	got, err := store.Resolve(context.Background(), "openai/local")
	if err != nil || got != secret {
		t.Fatalf("resolve = %q, %v", got, err)
	}
	encoded, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("marshal store: %v", err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("secret leaked from store JSON: %s", encoded)
	}
	if strings.Contains(store.String(), secret) || strings.Contains(fmt.Sprintf("%#v", store), secret) {
		t.Fatalf("secret leaked from store diagnostic")
	}
	if !strings.Contains(string(encoded), "openai/local") {
		t.Fatalf("configured reference missing from store JSON: %s", encoded)
	}

	store.Delete("openai/local")
	if _, err := store.Resolve(context.Background(), "openai/local"); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("missing credential error = %v, want ErrCredentialNotFound", err)
	}
}

func TestMemoryCredentialStoreRejectsCRLFAndInvalidReferences(t *testing.T) {
	store := NewMemoryCredentialStore()
	for _, tc := range []struct {
		name, ref, value string
	}{
		{"empty ref", "", "secret"},
		{"ref CRLF", "ref\n", "secret"},
		{"ref traversal", "../secret", "secret"},
		{"value CRLF", "safe", "secret\r\nnext"},
		{"empty value", "safe", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := store.Put(tc.ref, tc.value); err == nil {
				t.Fatal("invalid credential accepted")
			}
		})
	}
}

func TestMemoryCredentialStoreHonorsCancellation(t *testing.T) {
	store := NewMemoryCredentialStore()
	if err := store.Put("safe", "secret"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Resolve(ctx, "safe"); !errors.Is(err, context.Canceled) {
		t.Fatalf("resolve error = %v, want context.Canceled", err)
	}
}
