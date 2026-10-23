package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkerFaultAppliesToAnyRouteButReadinessIsIndependent(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "fault.json")
	if err := os.WriteFile(filename, []byte(`{"status":503,"delay":"1ms"}`), 0600); err != nil {
		t.Fatal(err)
	}
	h := newHandler(options{maxActive: 1, faultFile: filename})
	for path, want := range map[string]int{"/a/work": 503, "/b/work": 503, "/arbitrary/path": 503, "/readyz": 200} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != want {
			t.Fatalf("%s=%d", path, w.Code)
		}
	}
	// A different worker without the fault file stays healthy for the same path.
	other := newHandler(options{maxActive: 1})
	otherResponse := httptest.NewRecorder()
	other.ServeHTTP(otherResponse, httptest.NewRequest("GET", "/arbitrary/path", nil))
	if otherResponse.Code != 200 {
		t.Fatalf("fault leaked into independent worker: %d", otherResponse.Code)
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
	h := newHandler(options{maxActive: 1, delay: time.Millisecond})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/arbitrary/path?delay=2m", nil))
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	t.Setenv("DEMO_MAX_ACTIVE", "0")
	if _, err := optionsFromEnv(); err == nil {
		t.Fatal("invalid capacity accepted")
	}
}
