package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/fx"

	"junglegaming/challenge/internal/config"
)

func TestOIDCAuthenticationAndAuthorization(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	// A separate public issuer proves internal routing never weakens issuer checks.
	issuer := "http://identity.invalid/realms/test"
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/realms/test/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "jwks_uri": issuer + "/certs", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/realms/test/certs":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "RSA", "kid": "test", "use": "sig", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer idp.Close()
	c := config.Config{OIDCIssuer: issuer, OIDCBackchannel: idp.URL, OIDCAudience: "wagering-api", DependencyTimeout: time.Second}
	var a *Authenticator
	app := fx.New(fx.NopLogger, fx.Supply(c), fx.Provide(New), fx.Populate(&a))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	sign := func(changes map[string]any) string {
		claims := map[string]any{"iss": issuer, "aud": "wagering-api", "sub": "service-account", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(), "service_role": "provider", "provider_id": "provider-a"}
		for k, v := range changes {
			claims[k] = v
		}
		header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test"})
		payload, _ := json.Marshal(claims)
		unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
		hash := sha256.Sum256([]byte(unsigned))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
		if err != nil {
			t.Fatal(err)
		}
		return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig)
	}
	good := sign(nil)
	terminal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := FromContext(r.Context())
		if !ok || id.Subject == "" {
			t.Error("missing authenticated identity")
		}
		w.WriteHeader(204)
	})
	for _, tc := range []struct {
		name, token string
		handler     http.Handler
		want        int
	}{
		{"missing", "", terminal, 401}, {"malformed", "bad.token", terminal, 401},
		{"valid", good, terminal, 204}, {"wrong issuer", sign(map[string]any{"iss": "http://other.invalid"}), terminal, 401},
		{"wrong audience", sign(map[string]any{"aud": "another-api"}), terminal, 401},
		{"expired", sign(map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}), terminal, 401},
		{"missing subject", sign(map[string]any{"sub": ""}), terminal, 401},
		{"invalid signature", good[:len(good)-8] + "AAAAAAAA", terminal, 401},
		{"missing role", sign(map[string]any{"service_role": ""}), terminal, 403},
		{"missing provider", sign(map[string]any{"provider_id": ""}), terminal, 403},
		{"same provider", good, RequireProvider("provider-a", terminal), 204},
		{"different provider", good, RequireProvider("provider-b", terminal), 403},
		{"provider denied internal", good, RequireInternal(terminal), 403},
		{"internal accepted", sign(map[string]any{"service_role": "internal", "provider_id": ""}), RequireInternal(terminal), 204},
		{"ambiguous identity", sign(map[string]any{"service_role": "internal"}), terminal, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			w := httptest.NewRecorder()
			a.Authenticate(tc.handler).ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d, want %d", w.Code, tc.want)
			}
		})
	}
}
func TestAuthorizationFailsWithoutAuthentication(t *testing.T) {
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unauthorized handler called") })
	for _, h := range []http.Handler{RequireInternal(next), RequireProvider("provider-a", next)} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		if w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
}
