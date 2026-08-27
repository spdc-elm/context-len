package workspace

import (
	"net/http"
	"net/http/httptest"
	"strings"
)

func jsonRequest(method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}
