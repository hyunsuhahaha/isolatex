package gateway

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"isolatex/internal/config"
)

func TestBulkheadsRetainReservedCapacityUnderOverflow(t *testing.T) {
	for _, budget := range []int{4, 8} {
		t.Run(fmt.Sprintf("budget_%d", budget), func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			g, s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}), func(c *config.Config) {
				c.MaxAdmitted = budget
				for name, b := range c.Bulkheads {
					b.QueueTimeout = 10 * time.Second
					b.RequestTimeout = 15 * time.Second
					c.Bulkheads[name] = b
				}
			})
			// Registered after server cleanup so failures also unblock the backend.
			t.Cleanup(unblock)
			results := make(chan int, 4)
			request := func(path string) {
				resp, err := s.Client().Get(s.URL + path)
				if err != nil {
					results <- 0
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				results <- resp.StatusCode
			}
			// Fill ERP's execution slot and queue before normal traffic arrives.
			for i := 0; i < 2; i++ {
				go request("/api/erp")
			}
			external, normal := g.groups[0], g.groups[1]
			eventually(t, func() bool { a, q := external.gate.snapshot(); return a == 1 && q == 1 })
			// Rejected ERP requests must never compete for normal's reservation.
			var flood sync.WaitGroup
			failures := make(chan string, 32)
			for i := 0; i < 32; i++ {
				flood.Add(1)
				go func() {
					defer flood.Done()
					resp, err := s.Client().Get(s.URL + "/api/erp")
					if err != nil {
						failures <- err.Error()
						return
					}
					body, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					if resp.StatusCode != 503 || !strings.Contains(string(body), "queue_full") {
						failures <- fmt.Sprintf("ERP overflow: HTTP %d %s", resp.StatusCode, body)
					}
				}()
			}
			for i := 0; i < 2; i++ {
				go request("/api/orders")
			}
			eventually(t, func() bool { a, q := normal.gate.snapshot(); return a == 1 && q == 1 })
			flood.Wait()
			close(failures)
			for failure := range failures {
				t.Error(failure)
			}
			// Spare global budget is not borrowed by a saturated group either.
			if got := code(t, s.Client(), s.URL+"/api/orders"); got != 503 {
				t.Fatalf("normal overflow = %d", got)
			}
			unblock()
			for i := 0; i < 4; i++ {
				if got := <-results; got != 200 {
					t.Errorf("reserved request = %d", got)
				}
			}
			eventually(t, func() bool {
				for _, b := range g.groups {
					a, q := b.gate.snapshot()
					if a != 0 || q != 0 {
						return false
					}
				}
				return true
			})
		})
	}
}
