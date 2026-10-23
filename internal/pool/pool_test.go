package pool

import "testing"

func TestLeastActiveAndReadiness(t *testing.T) {
	p := New([]string{"http://127.0.0.1:1", "http://127.0.0.1:2"}, false)
	if u, _ := p.Pick(); u != nil {
		t.Fatal("picked unready worker")
	}
	p.Set(0, true, 10, "ready")
	p.Set(1, true, 11, "ready")
	u, a := p.Pick()
	v, b := p.Pick()
	if u == v {
		t.Fatal("did not spread equal load")
	}
	a()
	a() // releases are idempotent
	w, c := p.Pick()
	if w != u {
		t.Fatal("did not select least active")
	}
	b()
	c()
	for _, worker := range p.Snapshot() {
		if worker.Active != 0 {
			t.Fatal("active leak")
		}
	}
}

func TestHealthHysteresis(t *testing.T) {
	p := New([]string{"http://127.0.0.1:1"}, false)
	p.Set(0, false, 10, "starting")
	p.Health(0, true, 3, 2)
	if p.Ready() != 0 {
		t.Fatal("became ready before success threshold")
	}
	p.Health(0, true, 3, 2)
	if p.Ready() != 1 {
		t.Fatal("did not become ready")
	}
	p.Health(0, false, 3, 2)
	p.Health(0, false, 3, 2)
	if p.Ready() != 1 {
		t.Fatal("became unhealthy before failure threshold")
	}
	p.Health(0, true, 3, 2)
	p.Health(0, false, 3, 2)
	p.Health(0, false, 3, 2)
	if p.Ready() != 1 {
		t.Fatal("success did not reset failure streak")
	}
	p.Health(0, false, 3, 2)
	if p.Ready() != 0 {
		t.Fatal("failed threshold did not remove worker")
	}
	p.Health(0, true, 3, 2)
	if p.Ready() != 0 {
		t.Fatal("recovered too early")
	}
	p.Health(0, true, 3, 2)
	if p.Ready() != 1 {
		t.Fatal("did not recover")
	}
}

func TestOldProcessCompletionCannotChangeReplacementLoad(t *testing.T) {
	p := New([]string{"http://127.0.0.1:1"}, false)
	p.Set(0, true, 10, "ready")
	_, oldRelease := p.Pick()
	p.Set(0, false, 0, "backoff")
	p.Set(0, false, 0, "starting")
	p.Set(0, true, 11, "ready")
	if got := p.Snapshot()[0].Active; got != 0 {
		t.Fatalf("replacement inherited old process load: %d", got)
	}
	_, newRelease := p.Pick()
	oldRelease()
	oldRelease()
	if got := p.Snapshot()[0].Active; got != 1 {
		t.Fatalf("old completion changed replacement load: %d", got)
	}
	newRelease()
	if got := p.Snapshot()[0].Active; got != 0 {
		t.Fatalf("replacement load leaked: %d", got)
	}
}

func TestHealthRecoveryKeepsInFlightLoad(t *testing.T) {
	p := New([]string{"http://127.0.0.1:1"}, false)
	p.Set(0, true, 10, "ready")
	_, release := p.Pick()
	p.Health(0, false, 1, 1)
	p.Health(0, true, 1, 1)
	if got := p.Snapshot()[0].Active; got != 1 {
		t.Fatalf("same process recovery lost in-flight load: %d", got)
	}
	release()
	if got := p.Snapshot()[0].Active; got != 0 {
		t.Fatalf("same process completion leaked: %d", got)
	}
}
