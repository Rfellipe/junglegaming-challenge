package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"junglegaming/challenge/internal/config"
)

var Module = fx.Module("postgres", fx.Provide(New))

type Database struct{ pool *pgxpool.Pool }

// Pool is available after successful Fx startup. Future repositories share it.
func (d *Database) Pool() *pgxpool.Pool { return d.pool }
func (d *Database) Ping(ctx context.Context) error {
	if d.pool == nil {
		return fmt.Errorf("PostgreSQL has not started")
	}
	return d.pool.Ping(ctx)
}
func New(lc fx.Lifecycle, c config.Config) (*Database, error) {
	cfg, err := pgxpool.ParseConfig(c.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid PostgreSQL configuration")
	}
	cfg.MaxConns = 10
	database := &Database{}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, c.DependencyTimeout)
			defer cancel()
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				return fmt.Errorf("create PostgreSQL pool")
			}
			database.pool = pool
			if err := pool.Ping(ctx); err != nil {
				pool.Close()
				return fmt.Errorf("PostgreSQL startup check failed")
			}
			return nil
		},
		OnStop: func(context.Context) error { database.pool.Close(); return nil },
	})
	return database, nil
}
