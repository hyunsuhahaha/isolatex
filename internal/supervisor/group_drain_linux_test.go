//go:build linux

package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func groupStatFixture(group int, state string, threads int) string {
	fields := strings.Fields(state + " 1 " + strconv.Itoa(group) + " 0 0 0 0 0 0 0 0 0 0 0 0 0 0 " + strconv.Itoa(threads))
	return "42 (name ) with spaces\nand parens) " + strings.Join(fields, " ") + "\n"
}

func TestLinuxGroupStatDistinguishesZombieLeaderWithLiveThreads(t *testing.T) {
	for _, tc := range []struct {
		state   string
		threads int
		stopped bool
	}{
		{"R", 1, false}, {"D", 1, false}, {"T", 1, false}, {"Z", 1, true}, {"Z", 2, false}, {"X", 0, true},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.state, tc.threads), func(t *testing.T) {
			group, stopped, err := linuxProcessStat([]byte(groupStatFixture(123, tc.state, tc.threads)))
			if err != nil || group != 123 || stopped != tc.stopped {
				t.Fatal(group, stopped, err)
			}
		})
	}
	for _, value := range []string{"", "42 (missing) Z", strings.Repeat("x", 4097), groupStatFixture(123, "Z", -1), groupStatFixture(-1, "Z", 1)} {
		if _, _, err := linuxProcessStat([]byte(value)); err == nil {
			t.Fatalf("malformed stat accepted: %.80q", value)
		}
	}
}

func TestLinuxGroupInventoryFailsClosedAndSkipsOtherGroups(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, strconv.Itoa(os.Getpid()))
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	stat := filepath.Join(dir, "stat")
	group := syscall.Getpgrp()
	for _, tc := range []struct {
		state   string
		threads int
		live    bool
	}{{"R", 1, true}, {"Z", 1, false}, {"Z", 2, true}} {
		if err := os.WriteFile(stat, []byte(groupStatFixture(group, tc.state, tc.threads)), 0600); err != nil {
			t.Fatal(err)
		}
		live, members, err := linuxGroupMembers(root, group, time.Now().Add(time.Second))
		if err != nil || live != tc.live || members != 1 {
			t.Fatal(live, members, err)
		}
	}
	if err := os.WriteFile(stat, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := linuxGroupMembers(root, group, time.Now().Add(time.Second)); err == nil {
		t.Fatal("unreadable membership accepted")
	}
	// Unrelated processes require no stat read or permissions, and receive no signal.
	live, members, err := linuxGroupMembers(root, group+1, time.Now().Add(time.Second))
	if err != nil || live || members != 0 {
		t.Fatal(live, members, err)
	}
	if _, _, err := linuxGroupMembers(root, group, time.Now().Add(-time.Second)); err == nil {
		t.Fatal("expired scan deadline accepted")
	}
	if err := os.Remove(stat); err != nil {
		t.Fatal(err)
	}
	if live, members, err := linuxGroupMembers(root, group, time.Now().Add(time.Second)); err != nil || live || members != 0 {
		t.Fatal(live, members, err)
	}
	if _, _, err := linuxGroupMembers(filepath.Join(root, "missing"), group, time.Now().Add(time.Second)); err == nil {
		t.Fatal("missing inventory accepted")
	}
}

func TestLinuxGroupDrainCannotConfirmLiveGroup(t *testing.T) {
	// Query our own group without ever signaling it. A live group must not be
	// declared stopped just because the confirmation budget elapsed.
	started := time.Now()
	if err := waitLinuxProcessGroup(syscall.Getpgrp()); err == nil {
		t.Fatal("live group exit confirmed")
	}
	if time.Since(started) > 4*time.Second {
		t.Fatal("process group polling did not remain bounded")
	}
}
