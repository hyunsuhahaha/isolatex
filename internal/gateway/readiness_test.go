package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGroupReadinessDoesNotRemoveHealthyGroups(t *testing.T) {
	g, public := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	admin := httptest.NewServer(g.Admin())
	defer admin.Close()
	check := func(path string, want int) {
		t.Helper()
		if got := code(t, admin.Client(), admin.URL+path); got != want {
			t.Fatalf("%s = %d, want %d", path, got, want)
		}
	}
	check("/readyz", 200)
	check("/readyz/normal", 200)
	check("/readyz/isolated", 200)
	check("/readyz/not-configured", 404)
	g.groups[0].pool.Set(0, false, 0, "failed")
	check("/readyz", 503)
	check("/readyz/normal", 200)
	check("/readyz/isolated", 503)
	if got := code(t, public.Client(), public.URL+"/api/orders"); got != 200 {
		t.Fatal("healthy request affected by another group's readiness")
	}
	g.groups[1].pool.Set(0, false, 0, "unhealthy")
	g.groups[0].pool.Set(0, true, 123, "ready")
	check("/readyz/normal", 503)
	check("/readyz/isolated", 200)
	check("/readyz", 503)
	g.Drain()
	check("/readyz/normal", 503)
	check("/readyz/isolated", 503)
	check("/healthz", 200)
	check("/readyz/not-configured", 404)
}
