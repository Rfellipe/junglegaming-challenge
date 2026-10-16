package app

import (
	"log/slog"
	"os"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"junglegaming/challenge/internal/auth"
	"junglegaming/challenge/internal/config"
	"junglegaming/challenge/internal/httpapi"
	"junglegaming/challenge/internal/messaging"
	"junglegaming/challenge/internal/postgres"
	"junglegaming/challenge/internal/wagering"
)

func Modules() fx.Option {
	return fx.Options(
		fx.Provide(func() *slog.Logger { return slog.New(slog.NewJSONHandler(os.Stdout, nil)) }),
		fx.WithLogger(func(l *slog.Logger) fxevent.Logger { return &fxevent.SlogLogger{Logger: l} }),
		postgres.Module, messaging.Module, auth.Module, wagering.Module, messaging.WorkersModule,
		httpapi.Module,
	)
}

func New() *fx.App {
	c, err := config.Load()
	if err != nil {
		return fx.New(fx.Error(err))
	}
	return fx.New(fx.Supply(c), Modules(), fx.StartTimeout(c.StartupTimeout), fx.StopTimeout(c.ShutdownTimeout))
}
