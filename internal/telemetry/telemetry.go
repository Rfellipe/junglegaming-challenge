// Package telemetry provides bounded-cardinality, process-local Prometheus metrics.
package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
)

type correlationKey struct{}

func WithCorrelation(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey{}, id)
}
func Correlation(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey{}).(string)
	return id
}

type Metrics struct {
	mu     sync.Mutex
	values map[string]float64
}

func New() *Metrics { return &Metrics{values: make(map[string]float64)} }

// Names and labels must be constants chosen by the application, never user identifiers.
func (m *Metrics) Add(name string, value float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[name] += value
}
func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	keys := make([]string, 0, len(m.values))
	for k := range m.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "jungle_%s %g\n", k, m.values[k])
	}
}
