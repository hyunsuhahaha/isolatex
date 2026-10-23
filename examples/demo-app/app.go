package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type options struct {
	maxActive             int
	normalDelay, erpDelay time.Duration
	faultFile             string
}

// A file applies to all replicas given the same DEMO_FAULT_FILE. Readiness is
// independent of ERP availability unless ready=false is explicitly injected.
type fault struct {
	Ready              *bool  `json:"ready,omitempty"`
	ERPDelay           string `json:"erp_delay,omitempty"`
	ERPStatus          int    `json:"erp_status,omitempty"`
	IgnoreCancellation bool   `json:"ignore_cancellation,omitempty"`
}

func optionsFromEnv() (options, error) {
	o := options{maxActive: 1, normalDelay: 5 * time.Millisecond, erpDelay: 50 * time.Millisecond, faultFile: os.Getenv("DEMO_FAULT_FILE")}
	var err error
	if raw := os.Getenv("DEMO_MAX_ACTIVE"); raw != "" {
		o.maxActive, err = strconv.Atoi(raw)
		if err != nil || o.maxActive < 1 || o.maxActive > 10000 {
			return o, fmt.Errorf("DEMO_MAX_ACTIVE must be 1..10000")
		}
	}
	for key, dst := range map[string]*time.Duration{"DEMO_NORMAL_DELAY": &o.normalDelay, "DEMO_ERP_DELAY": &o.erpDelay} {
		if raw := os.Getenv(key); raw != "" {
			*dst, err = parseDelay(raw)
			if err != nil {
				return o, fmt.Errorf("%s: %w", key, err)
			}
		}
	}
	return o, nil
}

func parseDelay(raw string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 || d > time.Minute {
		return 0, fmt.Errorf("delay must be 0..1m")
	}
	return d, nil
}

func readFault(filename string) (fault, error) {
	var f fault
	if filename == "" {
		return f, nil
	}
	data, err := os.ReadFile(filename)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if len(data) > 4096 {
		return f, fmt.Errorf("fault file exceeds 4 KiB")
	}
	if err = json.Unmarshal(data, &f); err != nil {
		return f, err
	}
	if f.ERPStatus != 0 && (f.ERPStatus < 400 || f.ERPStatus > 599) {
		return f, fmt.Errorf("erp_status must be 400..599")
	}
	return f, nil
}

func newHandler(o options) http.Handler {
	capacity := make(chan struct{}, o.maxActive)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, err := readFault(o.faultFile)
		if err != nil {
			http.Error(w, "invalid demo fault file", 503)
			return
		}
		if r.URL.Path == "/readyz" {
			if f.Ready != nil && !*f.Ready {
				w.WriteHeader(503)
				return
			}
			w.WriteHeader(200)
			return
		}
		erp := r.URL.Path == "/api/erp" || strings.HasPrefix(r.URL.Path, "/api/erp/")
		if erp && r.URL.Query().Get("crash") == "1" {
			os.Exit(23)
		}
		delay := o.normalDelay
		ignoreCancel := false
		if erp {
			delay = o.erpDelay
			if f.ERPDelay != "" {
				delay, err = parseDelay(f.ERPDelay)
			}
			if raw := r.URL.Query().Get("delay"); raw != "" {
				delay, err = parseDelay(raw)
			}
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			ignoreCancel = f.IgnoreCancellation || r.URL.Query().Get("ignore_cancel") == "1"
		}
		select {
		case capacity <- struct{}{}:
			defer func() { <-capacity }()
		case <-r.Context().Done():
			return
		}
		t := time.NewTimer(delay)
		defer t.Stop()
		if ignoreCancel {
			<-t.C
		} else {
			select {
			case <-t.C:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if erp && f.ERPStatus != 0 {
			w.WriteHeader(f.ERPStatus)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"pool": os.Getenv("APP_POOL"), "pid": os.Getpid(), "path": r.URL.Path})
	})
}
