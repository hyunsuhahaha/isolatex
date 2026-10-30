//go:build windows

package supervisor

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCPUReportRejectsMissingTrialsAndGoFailure(t *testing.T) {
	for _, mode := range []string{"missing_samples", "malformed_samples", "go_failure"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			fakeGo := filepath.Join(dir, "fake-go.cmd")
			stub := "@echo off\r\nif \"%1\"==\"version\" (\r\necho go version fixture windows/amd64\r\nexit /b 0\r\n)\r\n"
			stub += "if \"%1\"==\"env\" (\r\necho {\"GOOS\":\"windows\"}\r\nexit /b 0\r\n)\r\n"
			if mode != "go_failure" {
				for _, trial := range []string{"unlimited", "cpu_only", "combined"} {
					if mode == "malformed_samples" {
						stub += `echo {"Action":"output","Output":"cpu_sample mode=` + trial + ` {}"}` + "\r\n"
					}
					stub += `echo {"Action":"pass","Test":"TestWorkerCPUBudgetContainsDescendantLoad/` + trial + `"}` + "\r\n"
				}
				stub += "exit /b 0\r\n"
			} else {
				stub += "echo fixture build failure 1>&2\r\nexit /b 7\r\n"
			}
			if err := os.WriteFile(fakeGo, []byte(stub), 0600); err != nil {
				t.Fatal(err)
			}
			outputDir := filepath.Join(dir, "reports")
			script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "check-cpu-windows.ps1"))
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("pwsh", "-NoProfile", "-File", script, "-GoBinary", fakeGo, "-Count", "1", "-OutputDirectory", outputDir)
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("incomplete CPU check passed: %s", output)
			}
			files, err := filepath.Glob(filepath.Join(outputDir, "*", "summary.json"))
			if err != nil || len(files) != 1 {
				t.Fatalf("missing failure report: files=%v error=%v output=%s", files, err, output)
			}
			data, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			var report struct {
				Passed      bool   `json:"passed"`
				Complete    bool   `json:"complete"`
				GoExit      int    `json:"go_exit_code"`
				Fingerprint string `json:"source_fingerprint_sha256"`
			}
			if err := json.Unmarshal(data, &report); err != nil {
				t.Fatal(err)
			}
			expectedGoExit := 0
			if mode == "go_failure" {
				expectedGoExit = 7
			}
			if report.Passed || report.Complete || report.GoExit != expectedGoExit || len(report.Fingerprint) != 64 {
				t.Fatalf("invalid failure report: %+v", report)
			}
			events, err := os.ReadFile(filepath.Join(filepath.Dir(files[0]), "events.jsonl"))
			if err != nil || (mode == "missing_samples" && !strings.Contains(string(events), `"Action":"pass"`)) {
				t.Fatalf("raw failure evidence missing: %v", err)
			}
			stderr, err := os.ReadFile(filepath.Join(filepath.Dir(files[0]), "stderr.log"))
			if err != nil || (mode == "go_failure" && !strings.Contains(string(stderr), "fixture build failure")) {
				t.Fatalf("raw stderr missing: %v", err)
			}
		})
	}
}
