package wagering

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.uber.org/fx"
	"junglegaming/challenge/internal/postgres"
)

var Module = fx.Module("wagering", fx.Provide(func(db *postgres.Database, logger *slog.Logger) *Service {
	s := New(db.Pool)
	s.Logger = logger
	return s
}), fx.Invoke(startReferenceWorker))

func startReferenceWorker(lc fx.Lifecycle, service *Service, logger *slog.Logger) {
	var cancel context.CancelFunc
	var done chan struct{}
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			var ctx context.Context
			ctx, cancel = context.WithCancel(context.Background())
			done = make(chan struct{})
			go func() {
				defer close(done)
				ticker := time.NewTicker(500 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
					}
					for i := 0; i < 32; i++ {
						attempt, cancelAttempt := context.WithTimeout(ctx, 5*time.Second)
						worked, err := service.RetryReference(attempt)
						cancelAttempt()
						if err != nil && !errors.Is(err, context.Canceled) {
							logger.Error("reference retry failed", "errorClass", "database")
						}
						if err != nil || !worked {
							break
						}
					}
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			cancel()
			select {
			case <-done:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
}
