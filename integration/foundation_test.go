//go:build integration

package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5"
	"go.uber.org/fx"

	"junglegaming/challenge/internal/app"
	"junglegaming/challenge/internal/auth"
	"junglegaming/challenge/internal/config"
	"junglegaming/challenge/internal/httpapi"
	"junglegaming/challenge/internal/messaging"
	"junglegaming/challenge/internal/migration"
	"junglegaming/challenge/internal/postgres"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	defaults := map[string]string{"DATABASE_URL": "postgres://wagering:wagering-local@localhost:5432/wagering?sslmode=disable", "OIDC_ISSUER_URL": "http://localhost:8081/realms/jungle", "SQS_ENDPOINT": "http://localhost:4566", "HTTP_ADDR": "127.0.0.1:0", "DEPENDENCY_TIMEOUT": "3s"}
	c, err := config.FromEnv(func(k string) string {
		if v := os.Getenv("TEST_" + k); v != "" {
			return v
		}
		return defaults[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func start(t *testing.T, a *fx.App) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
}
func token(t *testing.T, c config.Config, client, secret string) string {
	t.Helper()
	httpClient := &http.Client{Timeout: 5 * time.Second}
	response, err := httpClient.PostForm(c.OIDCIssuer+"/protocol/openid-connect/token", url.Values{"grant_type": {"client_credentials"}, "client_id": {client}, "client_secret": {secret}})
	if err != nil {
		t.Fatal("token endpoint unavailable:", err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("token endpoint returned %d", response.StatusCode)
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.AccessToken == "" {
		t.Fatal("missing access token")
	}
	return body.AccessToken
}
func TestRealOIDC(t *testing.T) {
	c := testConfig(t)
	var a *auth.Authenticator
	start(t, fx.New(fx.NopLogger, fx.Supply(c), auth.Module, fx.Populate(&a)))
	providerA := token(t, c, "provider-a", "provider-a-local")
	providerB := token(t, c, "provider-b", "provider-b-local")
	internal := token(t, c, "internal-service", "internal-service-local")
	terminal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.FromContext(r.Context())
		if !ok || id.Subject == "" {
			t.Error("missing identity")
		}
		w.WriteHeader(204)
	})
	for _, tc := range []struct {
		name, token string
		handler     http.Handler
		want        int
	}{
		{"missing", "", terminal, 401}, {"invalid", "invalid", terminal, 401},
		{"provider A", providerA, auth.RequireProvider("provider-a", terminal), 204},
		{"provider isolation", providerB, auth.RequireProvider("provider-a", terminal), 403},
		{"internal operation denied", providerA, auth.RequireInternal(terminal), 403},
		{"internal operation accepted", internal, auth.RequireInternal(terminal), 204},
		{"internal cannot impersonate provider", internal, auth.RequireProvider("provider-a", terminal), 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/", nil)
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			a.Authenticate(tc.handler).ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d, want %d", w.Code, tc.want)
			}
		})
	}
	t.Run("expired real token", func(t *testing.T) {
		parts := strings.Split(providerA, ".")
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		var claims struct {
			Exp int64 `json:"exp"`
		}
		if err := json.Unmarshal(payload, &claims); err != nil {
			t.Fatal(err)
		}
		delay := time.Until(time.Unix(claims.Exp+1, 0))
		if delay > 65*time.Second {
			t.Fatal("local token lifetime exceeds test bound")
		}
		if delay > 0 {
			t.Log("waiting for the real Keycloak token to expire")
			timer := time.NewTimer(delay)
			defer timer.Stop()
			<-timer.C
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+providerA)
		a.Authenticate(terminal).ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("expired token accepted")
		}
	})
}
func TestMigrations(t *testing.T) {
	c := testConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, c.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	name := fmt.Sprintf("foundation_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	}()
	u, _ := url.Parse(c.DatabaseURL)
	u.Path = "/" + name
	directory, err := filepath.Abs("../migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, direction := range []string{"up", "up", "down", "up"} {
		if err := migration.Run(u.String(), directory, direction); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname='wagering')").Scan(&exists); err != nil || !exists {
		t.Fatal("schema missing", err)
	}
	var version int
	var dirty bool
	if err := conn.QueryRow(ctx, "SELECT version, dirty FROM public.schema_migrations").Scan(&version, &dirty); err != nil || version != 3 || dirty {
		t.Fatal("migration state invalid", err)
	}
}
func TestQueueProvisioning(t *testing.T) {
	c := testConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(c.AWSRegion))
	if err != nil {
		t.Fatal(err)
	}
	client := sqs.NewFromConfig(cfg, func(o *sqs.Options) { o.BaseEndpoint = aws.String(c.SQSEndpoint) })
	var dlqARN string
	for _, name := range []string{c.QueueNames[1], c.QueueNames[0], c.QueueNames[2]} {
		u, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
		if err != nil {
			t.Fatal(err)
		}
		attributes, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: u.QueueUrl, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameAll}})
		if err != nil {
			t.Fatal(err)
		}
		if attributes.Attributes["FifoQueue"] != "true" || attributes.Attributes["ContentBasedDeduplication"] != "false" {
			t.Fatal("incorrect FIFO configuration")
		}
		if name == c.QueueNames[1] {
			dlqARN = attributes.Attributes["QueueArn"]
		}
		if name == c.QueueNames[0] {
			var policy struct {
				Target string `json:"deadLetterTargetArn"`
				Count  string `json:"maxReceiveCount"`
			}
			if err := json.Unmarshal([]byte(attributes.Attributes["RedrivePolicy"]), &policy); err != nil || policy.Target != dlqARN || policy.Count != "5" {
				t.Fatal("incorrect redrive policy", err)
			}
		}
	}
}
func TestLifecycleAndReadiness(t *testing.T) {
	c := testConfig(t)
	var server *httpapi.Server
	var pool *postgres.Database
	var queues *messaging.Client
	a := fx.New(fx.Supply(c), app.Modules(), fx.Populate(&server, &pool, &queues))
	// Test-only route holds a real request open to verify graceful draining.
	started := make(chan struct{})
	release := make(chan struct{})
	original := server.HTTP.Handler
	server.HTTP.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/test/drain" {
			close(started)
			<-release
			w.WriteHeader(204)
			return
		}
		original.ServeHTTP(w, r)
	})
	start(t, a)
	address := "http://" + server.HTTP.Addr
	// The configured ephemeral port is resolved through the listener's address.
	// Server.Addr is updated by the HTTP adapter after binding.
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(address + "/health/ready")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("not ready")
	}
	// A disconnected database pool is checked independently below; SQS outage
	// readiness is tested against a closed endpoint without changing shared queues.
	failedConfig := c
	failedConfig.SQSEndpoint = "http://127.0.0.1:1"
	failedConfig.DependencyTimeout = 500 * time.Millisecond
	var failedQueues *messaging.Client
	failedApp := fx.New(fx.NopLogger, fx.Supply(failedConfig), messaging.Module, fx.Populate(&failedQueues))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if failedApp.Start(ctx) == nil {
		t.Fatal("SQS outage did not fail startup")
	}
	handler := httpapi.HealthHandler(time.Second, map[string]httpapi.Check{"postgres": pool.Ping, "sqs": failedQueues.Check}, func() bool { return false })
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/health/ready", nil))
	if w.Code != 503 {
		t.Fatal("SQS outage did not affect readiness")
	}
	requestDone := make(chan error, 1)
	go func() {
		res, err := client.Get(address + "/test/drain")
		if err == nil {
			_, err = io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		stopped <- a.Stop(ctx)
	}()
	select {
	case err := <-stopped:
		close(release)
		t.Fatal("shutdown did not drain request", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if pool.Ping(context.Background()) == nil {
		t.Fatal("database pool still open")
	}
	w = httptest.NewRecorder()
	httpapi.HealthHandler(time.Second, map[string]httpapi.Check{"postgres": pool.Ping}, func() bool { return false }).ServeHTTP(w, httptest.NewRequest("GET", "/health/ready", nil))
	if w.Code != 503 {
		t.Fatal("database outage did not affect readiness")
	}
	if res, err := client.Get(address + "/health/live"); err == nil {
		res.Body.Close()
		t.Fatal("listener still open")
	}
}
func TestStartupFailureReleasesResources(t *testing.T) {
	c := testConfig(t)
	c.OIDCIssuer = "http://127.0.0.1:1/realms/missing"
	c.DependencyTimeout = time.Second
	var pool *postgres.Database
	a := fx.New(fx.Supply(c), app.Modules(), fx.Populate(&pool))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if a.Start(ctx) == nil {
		t.Fatal("expected failed discovery")
	}
	if pool.Ping(ctx) == nil {
		t.Fatal("pool not closed during rollback")
	}
}
