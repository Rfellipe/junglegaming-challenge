package messaging

import (
	"encoding/json"
	"testing"
	"time"

	"junglegaming/challenge/internal/domain"
	"junglegaming/challenge/internal/wagering"
)

func TestDecodeInput(t *testing.T) {
	money, _ := domain.Parse("1.00", "BRL")
	e := InputEnvelope{MessageID: "message", Type: "WagerTransactionRequested", OccurredAt: time.Now().UTC()}
	e.Data.Request = wagering.Request{ProviderID: "provider-a", ExternalID: "external", PlayerID: wagering.ID(), WalletID: wagering.ID(), RoundID: "round", GameID: "game", Kind: domain.Bet, Money: money}
	e.Data.IdempotencyKey = "key"
	b, _ := json.Marshal(e)
	_, hash, err := DecodeInput(string(b))
	if err != nil {
		t.Fatal(err)
	}
	indented, _ := json.MarshalIndent(e, "", "  ")
	_, other, err := DecodeInput(string(indented))
	if err != nil || other != hash {
		t.Fatal("format-dependent hash", err)
	}
	e.Data.IdempotencyKey = "changed"
	b, _ = json.Marshal(e)
	_, other, err = DecodeInput(string(b))
	if err != nil || other == hash {
		t.Fatal("key missing from inbox hash", err)
	}
	for _, body := range []string{"{}", string(b) + " {}", `{"unknown":true}`, `null`} {
		if _, _, err := DecodeInput(body); err == nil {
			t.Fatal("accepted", body)
		}
	}
}
