package domain

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"
)

func money(t *testing.T, amount string) Money {
	t.Helper()
	m, err := Parse(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func TestMoneyParsingAndJSON(t *testing.T) {
	for _, amount := range []string{"", "NaN", "Infinity", "1e2", "1", "1.0", "1.000", "+1.00", " 1.00", ".00", "1..00", "92233720368547758.08", "-92233720368547758.09"} {
		if _, err := Parse(amount, "BRL"); err == nil {
			t.Errorf("accepted %q", amount)
		}
	}
	for input, want := range map[string]string{"0.00": "0.00", "00025.00": "25.00", "-0.01": "-0.01", "92233720368547758.07": "92233720368547758.07", "-92233720368547758.08": "-92233720368547758.08"} {
		if got := money(t, input).Amount(); got != want {
			t.Errorf("%s: %s", input, got)
		}
	}
	for _, raw := range []string{`{"amount":25,"currency":"BRL"}`, `{"amount":"-0.00","currency":"BRL"}`, `{"amount":"-1.00","currency":"BRL"}`, `{"amount":"1.00","currency":"XYZ"}`, `null`, `{}`, `{"amount":"1.00","currency":"BRL","unexpected":1}`} {
		var m Money
		if err := json.Unmarshal([]byte(raw), &m); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	var m Money
	if json.Unmarshal([]byte(`{"amount":"00025.00","currency":"BRL"}`), &m) != nil {
		t.Fatal("valid JSON rejected")
	}
	b, err := json.Marshal(m)
	if err != nil || string(b) != `{"amount":"25.00","currency":"BRL"}` {
		t.Fatal(string(b), err)
	}
	if _, err := json.Marshal(Money{}); err == nil {
		t.Fatal("serialized uninitialized money")
	}
}
func TestMoneyArithmetic(t *testing.T) {
	max, _ := FromMinor(math.MaxInt64, "BRL")
	min, _ := FromMinor(math.MinInt64, "BRL")
	one, _ := FromMinor(1, "BRL")
	negative, _ := FromMinor(-1, "BRL")
	for _, f := range []func() (Money, error){func() (Money, error) { return max.Add(one) }, func() (Money, error) { return min.Add(negative) }, func() (Money, error) { return min.Sub(one) }, func() (Money, error) { return max.Sub(negative) }, min.Negate} {
		if _, err := f(); !errors.Is(err, ErrOverflow) {
			t.Fatal("missing overflow", err)
		}
	}
	zero, _ := Zero("BRL")
	got, err := min.Sub(min)
	if err != nil || got != zero {
		t.Fatal(got, err)
	}
	usd, _ := Parse("1.00", "USD")
	if _, err := one.Add(usd); !errors.Is(err, ErrCurrency) {
		t.Fatal(err)
	}
	if _, err := one.Sub(usd); !errors.Is(err, ErrCurrency) {
		t.Fatal(err)
	}
	if _, err := one.Compare(usd); !errors.Is(err, ErrCurrency) {
		t.Fatal(err)
	}
	if _, err := (Money{}).Add(one); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	got, err = money(t, "1.10").Add(money(t, "2.20"))
	if err != nil || got.Amount() != "3.30" {
		t.Fatal(got, err)
	}
	got, err = money(t, "1.10").Sub(money(t, "2.20"))
	if err != nil || got.Amount() != "-1.10" {
		t.Fatal(got, err)
	}
}
func TestWalletAndLedger(t *testing.T) {
	now := time.Now()
	w, err := NewWallet("wallet", "player", money(t, "100.00"), now)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Move(Debit, money(t, "80.00"), now); err != nil || w.Version() != 2 || w.Balance().Amount() != "20.00" {
		t.Fatal(w, err)
	}
	if err = w.Move(Debit, money(t, "80.00"), now); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatal(err)
	}
	if w.Balance().Amount() != "20.00" || w.Version() != 2 {
		t.Fatal("rejection mutated wallet")
	}
	if err = w.Move(Credit, money(t, "0.00"), now); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err = NewLedgerEntry("id", "wallet", "tx", Debit, money(t, "80.00"), money(t, "100.00"), w.Balance(), now); err != nil {
		t.Fatal(err)
	}
	if _, err = NewLedgerEntry("id", "wallet", "tx", Debit, money(t, "80.00"), money(t, "100.00"), money(t, "21.00"), now); err == nil {
		t.Fatal("invalid ledger accepted")
	}
	restored, err := RestoreWallet(w.ID(), w.PlayerID(), w.Balance(), w.Version(), w.CreatedAt(), w.UpdatedAt())
	if err != nil || restored.Balance() != w.Balance() || restored.Version() != 2 {
		t.Fatal("rehydration changed state", err)
	}
}
func TestTransactionRulesAndTransitions(t *testing.T) {
	now := time.Now()
	for _, kind := range []Kind{Bet, Win, Refund, Rollback} {
		ref := ""
		if kind == Refund || kind == Rollback {
			ref = "original"
		}
		if ValidateExternal(kind, money(t, "0.00"), ref) == nil {
			t.Fatalf("zero accepted: %s", kind)
		}
		if ValidateExternal(kind, money(t, "1.00"), ref) != nil {
			t.Fatalf("positive rejected: %s", kind)
		}
	}
	if ValidateExternal(Loss, money(t, "0.00"), "") != nil || ValidateExternal(Loss, money(t, "1.00"), "") == nil || ValidateExternal(Opening, money(t, "1.00"), "") == nil {
		t.Fatal("LOSS/OPENING policy")
	}
	for _, kind := range []Kind{Refund, Rollback} {
		if ValidateExternal(kind, money(t, "1.00"), "") == nil {
			t.Fatal("missing reference accepted")
		}
	}
	for _, terminal := range []Status{Processed, Rejected, Failed} {
		op, err := NewTransaction("id", Refund, money(t, "1.00"), "reference", now)
		if err != nil {
			t.Fatal(err)
		}
		if err = op.Transition(PendingReference, "", now); err != nil {
			t.Fatal(err)
		}
		failure := ""
		if terminal != Processed {
			failure = "CODE"
		}
		if err = op.Transition(terminal, failure, now); err != nil {
			t.Fatal(err)
		}
		if err = op.Transition(Processed, "", now); !errors.Is(err, ErrTransition) {
			t.Fatal("terminal state changed")
		}
	}
	op, _ := NewTransaction("id", Bet, money(t, "1.00"), "", now)
	if op.Transition(PendingReference, "", now) == nil {
		t.Fatal("unreferenced transaction can wait for reference")
	}
	if op.Transition(Rejected, "", now) == nil || op.Transition(Pending, "", now) == nil || op.Transition(Processed, "", now.Add(-time.Second)) == nil {
		t.Fatal("invalid transition accepted")
	}
}

func TestReferenceRules(t *testing.T) {
	base := TransactionData{TransactionID: "original", WalletID: "wallet", PlayerID: "player", ProviderID: "provider", RoundID: "round", GameID: "game", Kind: Bet, Money: money(t, "25.00")}
	current := base
	current.TransactionID = "reversal"
	current.Kind = Refund
	ref := Reference{TransactionData: base, Status: Processed, Direction: Debit}
	for _, tc := range []struct {
		kind      Kind
		original  Kind
		direction Direction
		want      Direction
	}{
		{Refund, Bet, Debit, Credit}, {Rollback, Bet, Debit, Credit}, {Rollback, Win, Credit, Debit}, {Rollback, Refund, Credit, Debit}, {Win, Bet, Debit, Credit},
	} {
		c, r := current, ref
		c.Kind, r.Kind, r.Direction = tc.kind, tc.original, tc.direction
		if got := ResolveReference(c, r); got.FailureCode != "" || got.Waiting || got.Direction != tc.want {
			t.Fatal(tc, got)
		}
	}
	for _, tc := range []struct {
		name   string
		change func(*TransactionData, *Reference)
		code   string
		wait   bool
	}{
		{"self", func(c *TransactionData, r *Reference) { c.TransactionID = r.TransactionID }, InvalidReference, false},
		{"provider", func(c *TransactionData, r *Reference) { c.ProviderID = "other" }, ReferenceMismatch, false},
		{"player", func(c *TransactionData, r *Reference) { c.PlayerID = "other" }, ReferenceMismatch, false},
		{"wallet", func(c *TransactionData, r *Reference) { c.WalletID = "other" }, ReferenceMismatch, false},
		{"round", func(c *TransactionData, r *Reference) { c.RoundID = "other" }, ReferenceMismatch, false},
		{"game", func(c *TransactionData, r *Reference) { c.GameID = "other" }, ReferenceMismatch, false},
		{"currency", func(c *TransactionData, r *Reference) { c.Money, _ = Parse("25.00", "USD") }, ReferenceMismatch, false},
		{"amount", func(c *TransactionData, r *Reference) { c.Money = money(t, "24.00") }, ReferenceAmountMismatch, false},
		{"kind", func(c *TransactionData, r *Reference) { r.Kind = Loss }, InvalidReferenceKind, false},
		{"pending", func(c *TransactionData, r *Reference) { r.Status = Pending }, "", true},
		{"pending reference", func(c *TransactionData, r *Reference) { r.Status = PendingReference }, "", true},
		{"rejected", func(c *TransactionData, r *Reference) { r.Status = Rejected }, ReferenceNotProcessed, false},
		{"failed", func(c *TransactionData, r *Reference) { r.Status = Failed }, ReferenceNotProcessed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, r := current, ref
			tc.change(&c, &r)
			got := ResolveReference(c, r)
			if got.FailureCode != tc.code || got.Waiting != tc.wait {
				t.Fatal(got)
			}
		})
	}
}
func TestEventContracts(t *testing.T) {
	now := time.Now()
	data := ProcessedData{TransactionData: TransactionData{TransactionID: "tx", WalletID: "wallet", PlayerID: "player", Kind: Opening, Money: money(t, "1.00")}, Balance: money(t, "1.00")}
	event := NewProcessedEvent("event", "tx", now, data)
	b, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err = json.Unmarshal(b, &result); err != nil {
		t.Fatal(err)
	}
	if event.Version != 1 || event.EventType != "WagerTransactionProcessed" || event.OccurredAt.Location() != time.UTC {
		t.Fatal(event)
	}
	payload := result["data"].(map[string]any)
	if _, exists := payload["providerId"]; exists {
		t.Fatal("internal event includes external identity")
	}
}
