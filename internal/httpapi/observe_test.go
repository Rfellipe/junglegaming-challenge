package httpapi

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"junglegaming/challenge/internal/telemetry"
)

func TestRequestLogsExcludeCredentialsAndPayload(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	h := observe(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(422); _, _ = w.Write([]byte("rejected")) }))
	r := httptest.NewRequest("POST", "/wallets/private-id?secret=hidden", strings.NewReader(`{"secret":"financial-payload"}`))
	r.Header.Set("Authorization", "Bearer private-token")
	r = r.WithContext(telemetry.WithCorrelation(r.Context(), "known-correlation"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 422 || w.Body.String() != "rejected" {
		t.Fatal(w)
	}
	for _, secret := range []string{"private-token", "financial-payload", "private-id", "hidden"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("sensitive request data logged")
		}
	}
	if !strings.Contains(logs.String(), `"status":422`) || !strings.Contains(logs.String(), "known-correlation") {
		t.Fatal(logs.String())
	}
}
