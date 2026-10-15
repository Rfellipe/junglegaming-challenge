package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPAddr          string
	DatabaseURL       string
	AWSRegion         string
	SQSEndpoint       string
	QueueNames        []string
	OIDCIssuer        string
	OIDCBackchannel   string
	OIDCAudience      string
	DependencyTimeout time.Duration
	StartupTimeout    time.Duration
	ShutdownTimeout   time.Duration
}

func Load() (Config, error) { return FromEnv(os.Getenv) }

func FromEnv(get func(string) string) (Config, error) {
	value := func(k, fallback string) string {
		if v := get(k); v != "" {
			return v
		}
		return fallback
	}
	c := Config{
		HTTPAddr: value("HTTP_ADDR", ":8080"), DatabaseURL: get("DATABASE_URL"),
		AWSRegion: value("AWS_REGION", "us-east-1"), SQSEndpoint: get("SQS_ENDPOINT"),
		QueueNames: []string{value("SQS_INPUT_QUEUE", "wager-transactions.fifo"), value("SQS_DLQ", "wager-transactions-dlq.fifo"), value("SQS_EVENTS_QUEUE", "wager-events.fifo")},
		OIDCIssuer: get("OIDC_ISSUER_URL"), OIDCBackchannel: get("OIDC_BACKCHANNEL_URL"), OIDCAudience: value("OIDC_AUDIENCE", "wagering-api"),
	}
	durations := []struct {
		key, fallback string
		target        *time.Duration
	}{
		{"DEPENDENCY_TIMEOUT", "3s", &c.DependencyTimeout}, {"STARTUP_TIMEOUT", "30s", &c.StartupTimeout}, {"SHUTDOWN_TIMEOUT", "15s", &c.ShutdownTimeout},
	}
	for _, d := range durations {
		v, err := time.ParseDuration(value(d.key, d.fallback))
		if err != nil || v <= 0 {
			return Config{}, fmt.Errorf("%s must be a positive duration", d.key)
		}
		*d.target = v
	}
	if _, port, err := net.SplitHostPort(c.HTTPAddr); err != nil {
		return Config{}, fmt.Errorf("HTTP_ADDR must be host:port")
	} else if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return Config{}, fmt.Errorf("HTTP_ADDR port is invalid")
	}
	db, err := url.Parse(c.DatabaseURL)
	if err != nil || db.Host == "" || (db.Scheme != "postgres" && db.Scheme != "postgresql") || db.Path == "" {
		return Config{}, fmt.Errorf("DATABASE_URL must be a PostgreSQL URL with a database")
	}
	for _, item := range []struct {
		name, value string
		optional    bool
	}{
		{"OIDC_ISSUER_URL", c.OIDCIssuer, false}, {"OIDC_BACKCHANNEL_URL", c.OIDCBackchannel, true}, {"SQS_ENDPOINT", c.SQSEndpoint, true},
	} {
		if item.optional && item.value == "" {
			continue
		}
		u, err := url.Parse(item.value)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return Config{}, fmt.Errorf("%s must be an HTTP(S) URL without credentials, query, or fragment", item.name)
		}
		if item.name == "OIDC_BACKCHANNEL_URL" && u.Path != "" && u.Path != "/" {
			return Config{}, fmt.Errorf("OIDC_BACKCHANNEL_URL must contain only an origin")
		}
	}
	seen := map[string]bool{}
	for _, name := range c.QueueNames {
		if seen[name] {
			return Config{}, fmt.Errorf("SQS queue names must be distinct")
		}
		seen[name] = true
	}
	return c, nil
}
