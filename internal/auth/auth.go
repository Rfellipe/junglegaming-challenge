package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"go.uber.org/fx"

	"junglegaming/challenge/internal/config"
)

var Module = fx.Module("auth", fx.Provide(New))

type Identity struct {
	Subject    string
	ProviderID string
	Role       string
}
type identityKey struct{}

func FromContext(ctx context.Context) (Identity, bool) {
	v, ok := ctx.Value(identityKey{}).(Identity)
	return v, ok
}

type Authenticator struct{ verifier *oidc.IDTokenVerifier }

// backchannelTransport changes only the network origin, never the expected issuer.
type backchannelTransport struct {
	base           http.RoundTripper
	issuer, target *url.URL
}

func (t backchannelTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme == t.issuer.Scheme && r.URL.Host == t.issuer.Host {
		r = r.Clone(r.Context())
		u := *r.URL
		r.URL = &u
		r.URL.Scheme = t.target.Scheme
		r.URL.Host = t.target.Host
		r.Host = ""
	}
	return t.base.RoundTrip(r)
}

func New(lc fx.Lifecycle, c config.Config) *Authenticator {
	a := &Authenticator{}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	client := &http.Client{Timeout: c.DependencyTimeout, Transport: transport}
	if c.OIDCBackchannel != "" {
		issuer, _ := url.Parse(c.OIDCIssuer)
		target, _ := url.Parse(c.OIDCBackchannel)
		client.Transport = backchannelTransport{base: transport, issuer: issuer, target: target}
	}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, c.DependencyTimeout)
			defer cancel()
			provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), c.OIDCIssuer)
			if err != nil {
				transport.CloseIdleConnections()
				return fmt.Errorf("OIDC discovery failed")
			}
			a.verifier = provider.VerifierContext(oidc.ClientContext(context.Background(), client), &oidc.Config{ClientID: c.OIDCAudience, SupportedSigningAlgs: []string{oidc.RS256}})
			return nil
		},
		OnStop: func(context.Context) error { transport.CloseIdleConnections(); return nil },
	})
	return a
}

func (a *Authenticator) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Fields(r.Header.Get("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || a.verifier == nil {
			unauthorized(w)
			return
		}
		token, err := a.verifier.Verify(r.Context(), parts[1])
		if err != nil {
			unauthorized(w)
			return
		}
		var claims struct {
			ProviderID string `json:"provider_id"`
			Role       string `json:"service_role"`
		}
		if token.Subject == "" || token.Claims(&claims) != nil {
			unauthorized(w)
			return
		}
		if (claims.Role != "provider" || claims.ProviderID == "") && (claims.Role != "internal" || claims.ProviderID != "") {
			forbidden(w)
			return
		}
		identity := Identity{Subject: token.Subject, ProviderID: claims.ProviderID, Role: claims.Role}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, identity)))
	})
}

// Provider access must be checked even on idempotent replays and lookups.
func RequireProvider(providerID string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := FromContext(r.Context())
		if !ok || id.Role != "provider" || id.ProviderID != providerID {
			forbidden(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func RequireInternal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := FromContext(r.Context())
		if !ok || id.Role != "internal" {
			forbidden(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="wagering-api"`)
	writeError(w, http.StatusUnauthorized, "unauthorized")
}
func forbidden(w http.ResponseWriter) { writeError(w, http.StatusForbidden, "forbidden") }
func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}
