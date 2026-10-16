//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/fx"
	"junglegaming/challenge/internal/app"
	"junglegaming/challenge/internal/domain"
	"junglegaming/challenge/internal/httpapi"
	"junglegaming/challenge/internal/migration"
	"junglegaming/challenge/internal/postgres"
	"junglegaming/challenge/internal/wagering"
)

func TestFinancialHTTPAndWorkerLifecycle(t *testing.T) {
	c := testConfig(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, c.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	name := fmt.Sprintf("http_finance_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	}()
	u, _ := url.Parse(c.DatabaseURL)
	u.Path = "/" + name
	c.DatabaseURL = u.String()
	directory, err := filepath.Abs("../migrations")
	if err != nil {
		t.Fatal(err)
	}
	if err = migration.Run(c.DatabaseURL, directory, "up"); err != nil {
		t.Fatal(err)
	}
	var server *httpapi.Server
	var db *postgres.Database
	var service *wagering.Service
	a := fx.New(fx.NopLogger, fx.Supply(c), app.Modules(), fx.Populate(&server, &db, &service))
	start(t, a)
	// Defers below stop the app before dropping its database; start also registers
	// idempotent cleanup to cover fatal assertions.
	defer func() {
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := a.Stop(stopCtx); err != nil {
			t.Error(err)
		}
	}()
	base := "http://" + server.HTTP.Addr
	internal := token(t, c, "internal-service", "internal-service-local")
	provider := token(t, c, "provider-a", "provider-a-local")
	other := token(t, c, "provider-b", "provider-b-local")
	client := &http.Client{Timeout: 5 * time.Second}
	call := func(method, path, bearer, key string, body any) (int, []byte) {
		t.Helper()
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r, err := http.NewRequest(method, base+path, bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		res, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, data
	}
	initial := map[string]any{"playerId": wagering.ID(), "initialBalance": map[string]string{"amount": "100.00", "currency": "BRL"}}
	for _, bearer := range []string{"", "invalid", provider} {
		status, _ := call("POST", "/wallets", bearer, "", initial)
		want := 401
		if bearer == provider {
			want = 403
		}
		if status != want {
			t.Fatalf("wallet auth: %d", status)
		}
	}
	status, b := call("POST", "/wallets", internal, "", initial)
	if status != 201 {
		t.Fatalf("opening %d: %s", status, b)
	}
	var w wagering.WalletView
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	if status, _ := call("POST", "/wallets", internal, "", initial); status != 409 {
		t.Fatal("duplicate wallet", status)
	}
	if status, _ := call("GET", "/wallets/"+w.ID, provider, "", nil); status != 403 {
		t.Fatal("wallet read exposed", status)
	}
	if status, _ := call("GET", "/wallets/"+w.ID, internal, "", nil); status != 200 {
		t.Fatal("wallet read", status)
	}
	money, _ := domain.Parse("25.00", "BRL")
	r := wagering.Request{ProviderID: "provider-a", ExternalID: wagering.ID(), PlayerID: w.PlayerID, WalletID: w.ID, RoundID: "round", GameID: "game", Kind: domain.Bet, Money: money}
	for _, bearer := range []string{"", other, internal} {
		status, _ := call("POST", "/wagering/transactions", bearer, r.ExternalID, r)
		want := 403
		if bearer == "" {
			want = 401
		}
		if status != want {
			t.Fatal("transaction authorization", status)
		}
	}
	if status, _ := call("POST", "/wagering/transactions", provider, "", r); status != 400 {
		t.Fatal("missing key", status)
	}
	status, b = call("POST", "/wagering/transactions", provider, r.ExternalID, r)
	if status != 200 {
		t.Fatalf("bet %d: %s", status, b)
	}
	var first wagering.Result
	if err := json.Unmarshal(b, &first); err != nil || first.Balance.Amount() != "75.00" {
		t.Fatal(string(b), err)
	}
	if status, _ := call("POST", "/wagering/transactions", other, r.ExternalID, r); status != 403 {
		t.Fatal("replay exposed", status)
	}
	status, b = call("POST", "/wagering/transactions", provider, r.ExternalID, r)
	var replay wagering.Result
	if err := json.Unmarshal(b, &replay); err != nil || status != 200 || !replay.IdempotentReplay || replay.TransactionID != first.TransactionID {
		t.Fatal(string(b), err)
	}
	changed := r
	changed.RoundID = "changed"
	if status, _ := call("POST", "/wagering/transactions", provider, r.ExternalID, changed); status != 409 {
		t.Fatal("payload conflict", status)
	}
	changed = r
	changed.ExternalID = wagering.ID()
	changed.Kind = domain.Opening
	if status, _ := call("POST", "/wagering/transactions", provider, changed.ExternalID, changed); status != 400 {
		t.Fatal("external opening accepted", status)
	}
	changed.Kind = domain.Bet
	changed.Money, _ = domain.Parse("80.00", "BRL")
	if status, _ := call("POST", "/wagering/transactions", provider, changed.ExternalID, changed); status != 422 {
		t.Fatal("business rejection", status)
	}
	// Accept a reference wait, stop all application components, create its original
	// using separate connections, and let a fresh Fx worker finish the durable work.
	bet := r
	bet.ExternalID = wagering.ID()
	refund := bet
	refund.ExternalID = wagering.ID()
	refund.Kind = domain.Refund
	refund.ReferenceExternalID = bet.ExternalID
	status, b = call("POST", "/wagering/transactions", provider, refund.ExternalID, refund)
	if status != 202 {
		t.Fatalf("pending %d: %s", status, b)
	}
	var pending wagering.Result
	if err := json.Unmarshal(b, &pending); err != nil {
		t.Fatal(err)
	}
	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := a.Stop(stopCtx); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	conn, err := pgx.Connect(ctx, c.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err = conn.Exec(ctx, `UPDATE wagering.transactions SET next_attempt_at=now() WHERE id=$1`, pending.TransactionID); err != nil {
		t.Fatal(err)
	}
	var restarted *wagering.Service
	var db2 *postgres.Database
	bapp := fx.New(fx.NopLogger, fx.Supply(c), app.Modules(), fx.Populate(&restarted, &db2))
	start(t, bapp)
	defer func() {
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := bapp.Stop(stopCtx); err != nil {
			t.Error(err)
		}
	}()
	if _, err = restarted.Process(ctx, bet, bet.ExternalID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		var status string
		if err := db2.Pool().QueryRow(ctx, `SELECT status FROM wagering.transactions WHERE id=$1`, pending.TransactionID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "PROCESSED" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restarted reference worker did not complete pending operation")
		}
		time.Sleep(20 * time.Millisecond)
	}
	result, err := restarted.Process(ctx, refund, refund.ExternalID)
	if err != nil || result.Status != domain.Processed || !result.IdempotentReplay || result.Balance.Amount() != "75.00" {
		t.Fatal(result, err)
	}
}
