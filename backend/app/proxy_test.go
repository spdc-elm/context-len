package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProxyHandlerRoute(t *testing.T) {
	s := NewServer(Config{ProxyHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })})
	r := httptest.NewRequest("POST", "http://proxy/v1/responses", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatalf("code %d", w.Code)
	}
}
