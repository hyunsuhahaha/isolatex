package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/logging"
)

type stalledHTTPLog struct {
	entered, resume chan struct{}
	once            sync.Once
}

func (w *stalledHTTPLog) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.resume
	return len(p), nil
}

func TestHTTPServerErrorsUseBoundedInstanceLogger(t *testing.T) {
	for _, listener := range []string{"public", "admin"} {
		t.Run(listener, func(t *testing.T) {
			ambient := &stalledHTTPLog{entered: make(chan struct{}), resume: make(chan struct{})}
			destination := &stalledHTTPLog{entered: make(chan struct{}), resume: make(chan struct{})}
			logs := logging.NewWriter(destination, 2, 1024)
			var releaseAmbient, releaseDestination sync.Once
			resume := func() {
				releaseAmbient.Do(func() { close(ambient.resume) })
				releaseDestination.Do(func() { close(destination.resume) })
			}
			previous := log.Writer()
			log.SetOutput(ambient)
			t.Cleanup(func() {
				resume()
				log.SetOutput(previous)
				_ = logs.Close(context.Background())
			})
			logger := slog.New(slog.NewJSONHandler(logs, nil))
			logger.Info("prime blocked destination")
			<-destination.entered
			handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("fixture handler failure") })
			server := httptest.NewUnstartedServer(handler)
			server.Config = newHTTPServer(handler, logger, listener)
			server.Start()
			defer server.Close()
			defer resume()
			client := &http.Client{Timeout: 2 * time.Second}
			defer client.CloseIdleConnections()
			for i := 0; i < 8; i++ {
				done := make(chan struct{})
				go func() {
					defer close(done)
					resp, err := client.Get(server.URL)
					if err == nil {
						_, _ = io.Copy(io.Discard, resp.Body)
						_ = resp.Body.Close()
					}
				}()
				select {
				case <-done:
				case <-time.After(750 * time.Millisecond):
					t.Fatal("HTTP error logging retained the failed connection")
				}
			}
			select {
			case <-ambient.entered:
				t.Fatal("server diagnostic reached ambient logger")
			default:
			}
			if logs.Dropped() == 0 {
				t.Fatal("stalled log destination did not produce bounded drops")
			}
		})
	}
}

func TestHTTPServerDiagnosticIncludesListenerAndSeverity(t *testing.T) {
	for _, listener := range []string{"public", "admin"} {
		t.Run(listener, func(t *testing.T) {
			var output bytes.Buffer
			logs := logging.NewWriter(&output, 8, 64<<10)
			logger := slog.New(slog.NewJSONHandler(logs, nil))
			handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("fixture diagnostic marker") })
			server := httptest.NewUnstartedServer(handler)
			server.Config = newHTTPServer(handler, logger, listener)
			server.Start()
			defer server.Close()
			client := &http.Client{Timeout: time.Second}
			defer client.CloseIdleConnections()
			resp, err := client.Get(server.URL)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := logs.Close(ctx); err != nil {
				t.Fatal(err)
			}
			var record struct{ Level, Listener, Msg string }
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatalf("diagnostic missing or not structured: %s (%v)", output.String(), err)
			}
			if record.Level != "ERROR" || record.Listener != listener || !strings.Contains(record.Msg, "fixture diagnostic marker") {
				t.Fatalf("diagnostic lost attribution: %+v", record)
			}
		})
	}
}
