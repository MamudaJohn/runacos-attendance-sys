package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRoutes(t *testing.T) {
	for _, tc := range []struct {
		method, path, body string
		code               int
	}{
		{"GET", "/", "Classroom attendance", 200}, {"GET", "/static/site.css", "font-family", 200},
		{"GET", "/healthz", `"ok"`, 200}, {"GET", "/readyz", `"ready"`, 200},
		{"GET", "/missing", "404", 404}, {"POST", "/healthz", "Method Not Allowed", 405},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			Handler(func(context.Context) error { return nil }).ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.body) {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("missing security header")
			}
		})
	}
}

func TestReadinessFailure(t *testing.T) {
	w := httptest.NewRecorder()
	Handler(func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("no deadline")
		}
		return errors.New("secret connection details")
	}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != 503 || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}
