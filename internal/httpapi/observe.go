package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"junglegaming/challenge/internal/telemetry"
)

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		w.ResponseWriter.WriteHeader(code)
	}
}
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func observe(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		tracked := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(tracked, r)
		status := tracked.status
		if status == 0 {
			status = 200
		}
		logger.Info("HTTP request", "correlationId", telemetry.Correlation(r.Context()), "method", r.Method, "status", status, "durationSeconds", time.Since(started).Seconds())
	})
}
