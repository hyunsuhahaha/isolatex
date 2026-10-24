package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNativeCLIConnectionBudgetPreservesAdminAndRecovers(t *testing.T) {
	dir := t.TempDir()
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	binary := filepath.Join(dir, "isolatex"+suffix)
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"+suffix), "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "protected\n") }))
	defer upstream.Close()
	public, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer public.Close()
	admin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	publicAddr, adminURL := public.Addr().String(), "http://"+admin.Addr().String()
	configuration := fmt.Sprintf(`version: 1
listen: %q
admin_listen: %q
max_connections: 2
bulkheads:
  normal:
    match: {path_prefix: /a}
    upstreams: [%q]
    max_concurrent: 1
    queue_size: 0
    queue_timeout: 100ms
    request_timeout: 1s
`, publicAddr, admin.Addr().String(), upstream.URL)
	filename := filepath.Join(dir, "isolatex.yaml")
	if err := os.WriteFile(filename, []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	logfile, err := os.Create(filepath.Join(dir, "runtime.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = logfile.Close()
		if t.Failed() {
			data, _ := os.ReadFile(logfile.Name())
			t.Logf("runtime log:\n%s", data)
		}
	})
	_ = public.Close()
	_ = admin.Close()
	cmd := exec.Command(binary, "-config", filename)
	cmd.Stdout, cmd.Stderr = logfile, logfile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true, Proxy: nil}}
	defer client.CloseIdleConnections()
	metrics := func() string {
		resp, err := client.Get(adminURL + "/metrics")
		if err != nil {
			return ""
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return string(data)
	}
	wait := func(predicate func() bool) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for !predicate() {
			if time.Now().After(deadline) {
				t.Fatal("connection budget condition timed out")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	wait(func() bool { return strings.Contains(metrics(), `isolatex_connections_limit{listener="public"} 2`) })
	if data := metrics(); !strings.Contains(data, `isolatex_connections_limit{listener="admin"} 64`) {
		t.Fatalf("admin budget missing: %s", data)
	}
	// One pre-header socket and one keep-alive socket consume both permits,
	// even though neither holds a classified request or queue slot.
	idle, err := net.DialTimeout("tcp", publicAddr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	keepAlive, err := net.DialTimeout("tcp", publicAddr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer keepAlive.Close()
	_ = keepAlive.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = fmt.Fprintf(keepAlive, "GET /a HTTP/1.1\r\nHost: test\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(keepAlive), nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(data) != "protected\n" {
		t.Fatalf("initial response: %d %q", resp.StatusCode, data)
	}
	wait(func() bool { return strings.Contains(metrics(), `isolatex_connections{listener="public"} 2`) })
	pending, err := net.DialTimeout("tcp", publicAddr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Close()
	_, _ = fmt.Fprintf(pending, "GET /a HTTP/1.1\r\nHost: test\r\nConnection: close\r\n\r\n")
	_ = pending.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	var one [1]byte
	if _, err := pending.Read(one[:]); err == nil {
		t.Fatal("request above the connection budget was served")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("expected backlog wait, got %v", err)
	}
	for _, path := range []string{"/healthz", "/readyz", "/status"} {
		resp, err := client.Get(adminURL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("admin %s unavailable under public saturation: %d", path, resp.StatusCode)
		}
	}
	_ = keepAlive.Close()
	_ = pending.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err = http.ReadResponse(bufio.NewReader(pending), nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(data) != "protected\n" {
		t.Fatalf("capacity did not recover: %d %q", resp.StatusCode, data)
	}
	_ = pending.Close()
	_ = idle.Close()
	wait(func() bool { return strings.Contains(metrics(), `isolatex_connections{listener="public"} 0`) })
}
