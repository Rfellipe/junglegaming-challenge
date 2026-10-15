package config

import (
	"strings"
	"testing"
	"time"
)

func TestConfiguration(t *testing.T) {
	base := map[string]string{"DATABASE_URL": "postgres://user:secret@localhost:5432/wagering", "OIDC_ISSUER_URL": "http://localhost:8081/realms/jungle"}
	cases := []struct{ name, key, value string }{
		{"missing database", "DATABASE_URL", ""}, {"wrong database scheme", "DATABASE_URL", "https://localhost/db"},
		{"missing issuer", "OIDC_ISSUER_URL", ""}, {"invalid issuer", "OIDC_ISSUER_URL", ":bad"},
		{"issuer credentials", "OIDC_ISSUER_URL", "https://user:secret@idp/realm"},
		{"invalid port", "HTTP_ADDR", ":65536"}, {"invalid address", "HTTP_ADDR", "8080"},
		{"zero timeout", "DEPENDENCY_TIMEOUT", "0s"}, {"negative timeout", "SHUTDOWN_TIMEOUT", "-1s"},
		{"invalid duration", "STARTUP_TIMEOUT", "tomorrow"}, {"bad SQS endpoint", "SQS_ENDPOINT", "file:///tmp/sqs"},
		{"backchannel path", "OIDC_BACKCHANNEL_URL", "http://keycloak:8080/realm"},
		{"duplicate queues", "SQS_EVENTS_QUEUE", "wager-transactions.fifo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := FromEnv(func(k string) string {
				if k == tc.key {
					return tc.value
				}
				return base[k]
			})
			if err == nil {
				t.Fatal("expected invalid configuration")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("configuration error leaked credentials")
			}
		})
	}
	c, err := FromEnv(func(k string) string { return base[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.DependencyTimeout != 3*time.Second || c.OIDCAudience != "wagering-api" {
		t.Fatal("unexpected defaults")
	}
}
