package main

import (
	"flag"
	"log/slog"
	"os"
	"path/filepath"

	"junglegaming/challenge/internal/migration"
)

func main() {
	directory := flag.String("dir", "migrations", "migration directory")
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if flag.NArg() != 1 {
		logger.Error("usage: migrate [-dir migrations] up|down")
		os.Exit(1)
	}
	abs, err := filepath.Abs(*directory)
	if err == nil {
		err = migration.Run(os.Getenv("DATABASE_URL"), abs, flag.Arg(0))
	}
	if err != nil {
		logger.Error("migration failed", "error", err)
		os.Exit(1)
	}
	logger.Info("migration complete", "direction", flag.Arg(0))
}
