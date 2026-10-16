package telemetry

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestConcurrentScrapeAndUpdates(t *testing.T) {
	m := New()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				m.Add("duplicates_total", 1)
				m.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/metrics", nil))
			}
		}()
	}
	wg.Wait()
	w := httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(w.Body.String(), "jungle_duplicates_total 1000\n") {
		t.Fatal(w.Body.String())
	}
}
