package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

var (
	ErrInvalid           = errors.New("invalid domain value")
	ErrCurrency          = errors.New("incompatible currencies")
	ErrOverflow          = errors.New("money overflow")
	ErrInsufficientFunds = errors.New("insufficient funds")
	ErrTransition        = errors.New("invalid transaction transition")
)

// Money is an immutable exact value. Its zero Go value is deliberately invalid.
type Money struct {
	minor    int64
	currency string
}

func FromMinor(n int64, currency string) (Money, error) {
	// Business operations support BRL; USD/EUR permit explicit mismatch tests.
	if currency != "BRL" && currency != "USD" && currency != "EUR" {
		return Money{}, ErrCurrency
	}
	return Money{n, currency}, nil
}
func Zero(currency string) (Money, error) { return FromMinor(0, currency) }
func Parse(amount, currency string) (Money, error) {
	if amount == "" {
		return Money{}, ErrInvalid
	}
	negative := strings.HasPrefix(amount, "-")
	s := amount
	if negative {
		s = s[1:]
	}
	p := strings.Split(s, ".")
	if len(p) != 2 || len(p[0]) == 0 || len(p[1]) != 2 {
		return Money{}, ErrInvalid
	}
	for _, c := range p[0] + p[1] {
		if c < '0' || c > '9' {
			return Money{}, ErrInvalid
		}
	}
	n, err := strconv.ParseInt(strings.TrimLeft(p[0]+p[1], "0"), 10, 64)
	if strings.Trim(p[0]+p[1], "0") == "" {
		n, err = 0, nil
	}
	if negative {
		// Parse the signed representation to include MinInt64 exactly.
		n, err = strconv.ParseInt("-"+p[0]+p[1], 10, 64)
	}
	if err != nil {
		return Money{}, ErrOverflow
	}
	return FromMinor(n, currency)
}
func (m Money) Valid() bool      { _, err := FromMinor(m.minor, m.currency); return err == nil }
func (m Money) Minor() int64     { return m.minor }
func (m Money) Currency() string { return m.currency }
func (m Money) Amount() string {
	s := strconv.FormatInt(m.minor, 10)
	negative := strings.HasPrefix(s, "-")
	if negative {
		s = s[1:]
	}
	for len(s) < 3 {
		s = "0" + s
	}
	s = s[:len(s)-2] + "." + s[len(s)-2:]
	if negative {
		s = "-" + s
	}
	return s
}
func (m Money) compatible(n Money) error {
	if !m.Valid() || !n.Valid() {
		return ErrInvalid
	}
	if m.currency != n.currency {
		return ErrCurrency
	}
	return nil
}
func (m Money) Add(n Money) (Money, error) {
	if err := m.compatible(n); err != nil {
		return Money{}, err
	}
	if (n.minor > 0 && m.minor > math.MaxInt64-n.minor) || (n.minor < 0 && m.minor < math.MinInt64-n.minor) {
		return Money{}, ErrOverflow
	}
	return FromMinor(m.minor+n.minor, m.currency)
}
func (m Money) Sub(n Money) (Money, error) {
	if err := m.compatible(n); err != nil {
		return Money{}, err
	}
	if (n.minor > 0 && m.minor < math.MinInt64+n.minor) || (n.minor < 0 && m.minor > math.MaxInt64+n.minor) {
		return Money{}, ErrOverflow
	}
	return FromMinor(m.minor-n.minor, m.currency)
}
func (m Money) Negate() (Money, error) {
	if !m.Valid() {
		return Money{}, ErrInvalid
	}
	if m.minor == math.MinInt64 {
		return Money{}, ErrOverflow
	}
	return FromMinor(-m.minor, m.currency)
}
func (m Money) Compare(n Money) (int, error) {
	if err := m.compatible(n); err != nil {
		return 0, err
	}
	if m.minor < n.minor {
		return -1, nil
	}
	if m.minor > n.minor {
		return 1, nil
	}
	return 0, nil
}
func (m Money) MarshalJSON() ([]byte, error) {
	if !m.Valid() {
		return nil, ErrInvalid
	}
	return json.Marshal(struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}{m.Amount(), m.currency})
}
func (m *Money) UnmarshalJSON(b []byte) error {
	var v struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&v); err != nil {
		return fmt.Errorf("money: %w", err)
	}
	if strings.HasPrefix(v.Amount, "-") {
		return ErrInvalid
	}
	n, err := Parse(v.Amount, v.Currency)
	if err != nil {
		return err
	}
	*m = n
	return nil
}
