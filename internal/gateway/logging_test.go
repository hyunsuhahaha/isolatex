package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
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

func TestProxyFailureUsesBoundedInstanceLogger(t *testing.T) {
	g, server := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/fault" {
			w.Header().Set("Content-Length", "32")
			w.Header().Set("Connection", "close")
			_, _ = io.WriteString(w, "x")
			return
		}
		_, _ = io.WriteString(w, "ok")
	}), func(c *config.Config) {
		b := c.Bulkheads["isolated"]
		b.RequestTimeout = 100 * time.Millisecond
		c.Bulkheads["isolated"] = b
	})
	// A serial test deliberately blocks the unrelated process-global logger.
	// Always unblock it before restoring its output (SetOutput takes its lock).
	ambient := &blockedLogDestination{entered: make(chan struct{}), resume: make(chan struct{})}
	destination := &blockedLogDestination{entered: make(chan struct{}), resume: make(chan struct{})}
	logs := logging.NewWriter(destination, 2, 1024)
	old := log.Writer()
	log.SetOutput(ambient)
	t.Cleanup(func() {
		close(ambient.resume)
		close(destination.resume)
		log.SetOutput(old)
		_ = logs.Close(context.Background())
	})
	g.log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	client := &http.Client{Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	if got := code(t, client, server.URL+"/api/work"); got != 200 {
		t.Fatal(got)
	}
	<-destination.entered
	for i := 0; i < 8; i++ {
		done := make(chan struct{})
		go func() {
			defer close(done)
			resp, err := client.Get(server.URL + "/api/fault")
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}()
		select {
		case <-done:
		case <-time.After(750 * time.Millisecond):
			t.Fatal("proxy body error blocked on ambient logging instead of finishing")
		}
		eventually(t, func() bool { a, q := g.groups[0].gate.snapshot(); return a == 0 && q == 0 })
		if got := code(t, client, server.URL+"/api/work"); got != 200 {
			t.Fatal(got)
		}
	}
	select {
	case <-ambient.entered:
		t.Fatal("proxy diagnostic reached ambient logger")
	default:
	}
	if logs.Dropped() == 0 {
		t.Fatal("bounded diagnostic queue did not account for dropped records")
	}
}

func TestProxyFailureDiagnosticIncludesGroupAndSeverity(t *testing.T) {
	g, server := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "32")
		w.Header().Set("Connection", "close")
		_, _ = io.WriteString(w, "x")
	}))
	var output bytes.Buffer
	logs := logging.NewWriter(&output, 8, 64<<10)
	g.log = slog.New(slog.NewJSONHandler(logs, nil))
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get(server.URL + "/api/fault")
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	eventually(t, func() bool { a, q := g.groups[0].gate.snapshot(); return a == 0 && q == 0 })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := logs.Close(ctx); err != nil {
		t.Fatal(err)
	}
	var record struct{ Level, Group, Msg string }
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("diagnostic missing or not structured: %s (%v)", output.String(), err)
	}
	if record.Level != "ERROR" || record.Group != "isolated" || !strings.Contains(record.Msg, "ReverseProxy read error") {
		t.Fatalf("diagnostic lost attribution: %+v", record)
	}
}
