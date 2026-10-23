package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/logging"
)

type blockedLogDestination struct {
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (d *blockedLogDestination) Write(p []byte) (int, error) {
	d.once.Do(func() { close(d.entered) })
	<-d.resume
	return len(p), nil
}

func TestStalledLogDestinationDoesNotBlockEitherGroup(t *testing.T) {
	g, server := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	destination := &blockedLogDestination{entered: make(chan struct{}), resume: make(chan struct{})}
	logs := logging.NewWriter(destination, 2, 1024)
	t.Cleanup(func() { close(destination.resume); _ = logs.Close(context.Background()) })
	g.log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	g.RegisterLogDrops(logs.Dropped)
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	if got := code(t, client, server.URL+"/api/orders"); got != 200 {
		t.Fatal(got)
	}
	<-destination.entered // The shared destination remains blocked throughout.
	for i := 0; i < 32; i++ {
		path := "/api/orders"
		if i%2 == 0 {
			path = "/api/fault"
		}
		if got := code(t, client, server.URL+path); got != 200 {
			t.Fatalf("request %d = %d", i, got)
		}
	}
	if logs.Dropped() == 0 {
		t.Fatal("full log queue was not accounted for")
	}
	families, err := g.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "isolatex_dropped_log_records_total" && family.Metric[0].GetCounter().GetValue() > 0 {
			return
		}
	}
	t.Fatal("missing dropped log metric")
}
