package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"go.uber.org/fx"

	"junglegaming/challenge/internal/auth"
	"junglegaming/challenge/internal/config"
	"junglegaming/challenge/internal/messaging"
	"junglegaming/challenge/internal/postgres"
)

var Module = fx.Module("http", fx.Provide(New), fx.Invoke(func(*Server) {}))

type Check func(context.Context) error

type Server struct {
	HTTP     *http.Server
	done     chan struct{}
	stopping atomic.Bool
}

func HealthHandler(timeout time.Duration, checks map[string]Check, stopping func() bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		respond(w, http.StatusOK, map[string]string{"status": "alive"})
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		if stopping() {
			respond(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		results := make(map[string]string, len(checks))
		status := http.StatusOK
		for name, check := range checks {
			if err := check(ctx); err != nil {
				results[name] = "unavailable"
				status = http.StatusServiceUnavailable
			} else {
				results[name] = "ready"
			}
		}
		respond(w, status, results)
	})
	return mux
}

func New(lc fx.Lifecycle, c config.Config, pool *postgres.Database, queues *messaging.Client, logger *slog.Logger, shutdown fx.Shutdowner, _ *auth.Authenticator) *Server {
	s := &Server{done: make(chan struct{})}
	s.HTTP = &http.Server{Addr: c.HTTPAddr, Handler: HealthHandler(c.DependencyTimeout, map[string]Check{"postgres": pool.Ping, "sqs": queues.Check}, s.stopping.Load), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 2*c.DependencyTimeout + time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			ln, err := net.Listen("tcp", c.HTTPAddr)
			if err != nil {
				return errors.New("HTTP listen failed")
			}
			s.HTTP.Addr = ln.Addr().String()
			go func() {
				defer close(s.done)
				if err := s.HTTP.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					logger.Error("HTTP server stopped unexpectedly")
					_ = shutdown.Shutdown(fx.ExitCode(1))
				}
			}()
			logger.Info("HTTP server started", "address", ln.Addr().String())
			return nil
		},
		OnStop: func(ctx context.Context) error {
			s.stopping.Store(true)
			err := s.HTTP.Shutdown(ctx)
			if err != nil {
				_ = s.HTTP.Close()
			}
			<-s.done
			return err
		},
	})
	return s
}
func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
