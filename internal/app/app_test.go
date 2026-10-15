package app

import (
	"testing"

	"go.uber.org/fx"

	"junglegaming/challenge/internal/config"
)

func TestComposition(t *testing.T) {
	c := config.Config{DatabaseURL: "postgres://user:local@localhost:5432/test", OIDCIssuer: "http://localhost:8081/realms/jungle"}
	if err := fx.ValidateApp(fx.Supply(c), Modules()); err != nil {
		t.Fatal(err)
	}
}
func TestInvalidConfigurationPreventsStartup(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("OIDC_ISSUER_URL", "")
	if New().Err() == nil {
		t.Fatal("expected configuration error")
	}
}
