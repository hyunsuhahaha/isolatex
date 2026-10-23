package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestERPFaultDoesNotChangeReadinessOrNormalRoute(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "fault.json")
	if err := os.WriteFile(filename, []byte(`{"erp_status":503,"erp_delay":"1ms"}`), 0600); err != nil {
		t.Fatal(err)
	}
	h := newHandler(options{maxActive: 1, faultFile: filename})
	for path, want := range map[string]int{"/api/erp": 503, "/api/erp-old": 200, "/api/orders": 200, "/readyz": 200} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != want {
			t.Fatalf("%s=%d", path, w.Code)
		}
	}
	if err := os.WriteFile(filename, []byte(`{"ready":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}

func TestInvalidDelayAndFaultConfiguration(t *testing.T) {
	h := newHandler(options{maxActive: 1, erpDelay: time.Millisecond})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/erp?delay=2m", nil))
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	t.Setenv("DEMO_MAX_ACTIVE", "0")
	if _, err := optionsFromEnv(); err == nil {
		t.Fatal("invalid capacity accepted")
	}
}
