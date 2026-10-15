package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealth(t *testing.T) {
	for _, tc := range []struct {
		name, path     string
		fail, stopping bool
		want           int
	}{
		{"live", "/health/live", true, false, 200}, {"ready", "/health/ready", false, false, 200},
		{"outage", "/health/ready", true, false, 503}, {"shutdown", "/health/ready", false, true, 503},
		{"unknown", "/wallets", false, false, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := HealthHandler(time.Second, map[string]Check{"postgres": func(context.Context) error {
				if tc.fail {
					return errors.New("password=secret")
				}
				return nil
			}}, func() bool { return tc.stopping })
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if w.Code != tc.want {
				t.Fatalf("got %d, want %d", w.Code, tc.want)
			}
			if strings.Contains(w.Body.String(), "secret") {
				t.Fatal("health leaked dependency details")
			}
		})
	}
}
func TestReadinessDeadline(t *testing.T) {
	h := HealthHandler(20*time.Millisecond, map[string]Check{"sqs": func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}, func() bool { return false })
	w := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/health/ready", nil))
	if w.Code != 503 || time.Since(start) > time.Second {
		t.Fatal("readiness did not honor deadline")
	}
}
