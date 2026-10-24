//go:build linux

package supervisor

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hyunsuhahaha/isolatex/internal/config"
)

func TestMemoryBudgetRejectsOrdinaryDirectoryBeforeWorkerStarts(t *testing.T) {
	parent := t.TempDir()
	marker := filepath.Join(t.TempDir(), "executed")
	cmd := exec.Command("sh", "-c", `printf started > "$1"`, "isolatex-marker", marker)
	control, err := startManagedProcessWithResources(cmd, &config.Resources{MemoryMaxBytes: 64 << 20, CgroupParent: parent})
	if err == nil {
		control.signal(true)
		_ = cmd.Wait()
		_ = control.close()
		t.Fatal("memory budget silently ignored on ordinary filesystem")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("worker executed despite unenforced budget: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed preflight left files: %v %+v", err, entries)
	}
}
