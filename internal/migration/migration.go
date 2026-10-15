package migration

import (
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

// Run applies all forward migrations, or reverses exactly one migration.
func Run(databaseURL, directory, direction string) error {
	if direction != "up" && direction != "down" {
		return fmt.Errorf("migration direction must be up or down")
	}
	u, err := url.Parse(databaseURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return fmt.Errorf("invalid PostgreSQL migration URL")
	}
	u.Scheme = "pgx5"
	q := u.Query()
	q.Set("connect_timeout", "10")
	q.Set("search_path", "public")
	q.Set("x-statement-timeout", "10000")
	u.RawQuery = q.Encode()
	m, err := migrate.New((&url.URL{Scheme: "file", Path: directory}).String(), u.String())
	if err != nil {
		return fmt.Errorf("initialize migrations failed; check database connectivity and migration directory")
	}
	defer m.Close()
	m.LockTimeout = 10 * time.Second
	if direction == "up" {
		err = m.Up()
	} else {
		err = m.Steps(-1)
	}
	if errors.Is(err, migrate.ErrNoChange) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("migration %s failed: %w", direction, err)
	}
	return nil
}
