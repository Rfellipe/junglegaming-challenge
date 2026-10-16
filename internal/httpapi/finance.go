package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"junglegaming/challenge/internal/auth"
	"junglegaming/challenge/internal/domain"
	"junglegaming/challenge/internal/wagering"
)

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return domain.ErrInvalid
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return domain.ErrInvalid
	}
	return nil
}
func financeError(w http.ResponseWriter, err error) {
	status, code := http.StatusServiceUnavailable, "temporarily_unavailable"
	switch {
	case errors.Is(err, domain.ErrInvalid), errors.Is(err, domain.ErrCurrency), errors.Is(err, domain.ErrOverflow):
		status, code = http.StatusBadRequest, "invalid_input"
	case errors.Is(err, wagering.ErrConflict):
		status, code = http.StatusConflict, "conflict"
	case errors.Is(err, wagering.ErrNotFound):
		status, code = http.StatusNotFound, "wallet_not_found"
	}
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "1")
	}
	respond(w, status, map[string]string{"error": code})
}
func FinanceHandler(a *auth.Authenticator, s *wagering.Service, timeout time.Duration) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /wallets", auth.RequireInternal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			PlayerID       string       `json:"playerId"`
			InitialBalance domain.Money `json:"initialBalance"`
		}
		if err := decode(w, r, &input); err != nil {
			financeError(w, err)
			return
		}
		wallet, err := s.CreateWallet(r.Context(), input.PlayerID, input.InitialBalance)
		if err != nil {
			financeError(w, err)
			return
		}
		respond(w, http.StatusCreated, wallet)
	})))
	mux.Handle("GET /wallets/{walletId}", auth.RequireInternal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wallet, err := s.Wallet(r.Context(), r.PathValue("walletId"))
		if err != nil {
			financeError(w, err)
			return
		}
		respond(w, http.StatusOK, wallet)
	})))
	mux.HandleFunc("POST /wagering/transactions", func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.FromContext(r.Context())
		if !ok || id.Role != "provider" {
			respond(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		var input wagering.Request
		if err := decode(w, r, &input); err != nil {
			financeError(w, err)
			return
		}
		if input.ProviderID != id.ProviderID {
			respond(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		result, err := s.Process(r.Context(), input, r.Header.Get("Idempotency-Key"))
		if err != nil {
			financeError(w, err)
			return
		}
		status := http.StatusOK
		if result.Status == domain.PendingReference {
			status = http.StatusAccepted
		}
		if result.Status == domain.Rejected {
			status = http.StatusUnprocessableEntity
		}
		respond(w, status, result)
	})
	return a.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	}))
}
