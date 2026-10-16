package wagering

import (
	"junglegaming/challenge/internal/domain"
	"strings"
	"testing"
)

func TestCanonicalHash(t *testing.T) {
	m, _ := domain.Parse("00025.00", "BRL")
	r := Request{ProviderID: "provider", ExternalID: "external", PlayerID: ID(), WalletID: ID(), RoundID: "round", GameID: "game", Kind: domain.Bet, Money: m}
	a, err := CanonicalHash(r)
	if err != nil {
		t.Fatal(err)
	}
	r.PlayerID = strings.ToUpper(r.PlayerID)
	r.WalletID = strings.ToUpper(r.WalletID)
	r.Money, _ = domain.Parse("25.00", "BRL")
	b, err := CanonicalHash(r)
	if err != nil || a != b {
		t.Fatal("equivalent payload hashes differ")
	}
	r.RoundID = "other"
	c, _ := CanonicalHash(r)
	if c == a {
		t.Fatal("changed payload not detected")
	}
}
