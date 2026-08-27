package proxy

import (
	"context-lens/backend/transport"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestTransparent(t *testing.T) {
	body := []byte(`{"model":"m","unknown":{"x":1}}`)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if string(got) != string(body) {
			t.Fatalf("body changed")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		w.Write(body)
	}))
	defer up.Close()
	u := mustURL(t, up.URL)
	tr, err := transport.New(transport.Config{BaseURL: u})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(NewHandler(tr))
	defer s.Close()
	req, _ := http.NewRequest("POST", s.URL+"/v1/responses?x=1%2F2", io.NopCloser(bytesReader(body)))
	req.Header.Set("Authorization", "client-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 201 || sha256.Sum256(got) != sha256.Sum256(body) {
		t.Fatalf("response changed")
	}
}
func mustURL(t *testing.T, s string) *url.URL {
	u, e := url.Parse(s)
	if e != nil {
		t.Fatal(e)
	}
	return u
}
func bytesReader(b []byte) io.Reader { return strings.NewReader(string(b)) }
