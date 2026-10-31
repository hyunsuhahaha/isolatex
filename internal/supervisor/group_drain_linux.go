//go:build linux

package supervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Kill is asynchronous. A process group may also persist after all its members
// become zombies, so kill(pgid, 0) alone cannot confirm runnable work has ended.
func waitLinuxProcessGroup(pgid int) error {
	deadline := time.Now().Add(time.Second)
	for {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		} else if err != nil {
			return fmt.Errorf("query worker process group: %w", err)
		}
		live, members, err := linuxGroupMembers("/proc", pgid, deadline)
		// procfs membership/state is not an atomic snapshot. A task may
		// disappear while being read. Require a later successful observation
		// or ESRCH, rather than making one transient scan error terminal.
		if err == nil && !live && members > 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return errors.Join(fmt.Errorf("worker process group %d exit unconfirmed", pgid), err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Stream directory entries instead of allocating a host-wide process list.
// Bound the scan's entries, stat size and wall-clock polling budget. Native
// filesystem calls themselves are not interruptible by this userspace deadline.
func linuxGroupMembers(root string, pgid int, deadline time.Time) (live bool, members int, err error) {
	directory, err := os.Open(root)
	if err != nil {
		return false, 0, fmt.Errorf("open process inventory: %w", err)
	}
	defer directory.Close()
	entries := 0
	for {
		batch, readErr := directory.ReadDir(128)
		for _, entry := range batch {
			entries++
			if entries > 65536 {
				return false, members, fmt.Errorf("process inventory exceeds 65536 entries")
			}
			if !time.Now().Before(deadline) {
				return false, members, fmt.Errorf("process inventory deadline exceeded")
			}
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid < 1 || !entry.IsDir() {
				continue
			}
			actualGroup, err := syscall.Getpgid(pid)
			if errors.Is(err, syscall.ESRCH) {
				continue
			}
			if err != nil {
				return false, members, fmt.Errorf("query process %d group: %w", pid, err)
			}
			if actualGroup != pgid {
				continue
			}
			file, err := os.Open(filepath.Join(root, entry.Name(), "stat"))
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
				continue
			}
			if err != nil {
				return false, members, fmt.Errorf("open process %s stat: %w", entry.Name(), err)
			}
			data, err := io.ReadAll(io.LimitReader(file, 4097))
			_ = file.Close()
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
				continue
			}
			if err != nil {
				return false, members, fmt.Errorf("read process %s stat: %w", entry.Name(), err)
			}
			group, stopped, err := linuxProcessStat(data)
			if err != nil {
				return false, members, fmt.Errorf("parse process %s stat: %w", entry.Name(), err)
			}
			if group == pgid {
				members++
				if !stopped {
					return true, members, nil
				}
			}
		}
		if readErr == io.EOF {
			return false, members, nil
		}
		if readErr != nil {
			return false, members, fmt.Errorf("read process inventory: %w", readErr)
		}
	}
}

func linuxProcessStat(data []byte) (group int, stopped bool, err error) {
	// comm can contain whitespace, newlines and parentheses. Fields after its
	// final ')' start with state (3), ppid (4), pgrp (5), ... num_threads (20).
	end := strings.LastIndex(string(data), ") ")
	if len(data) > 4096 || end < 0 {
		return 0, false, fmt.Errorf("invalid stat record")
	}
	fields := strings.Fields(string(data[end+2:]))
	if len(fields) < 18 || len(fields[0]) != 1 {
		return 0, false, fmt.Errorf("incomplete stat record")
	}
	group, err = strconv.Atoi(fields[2])
	if err != nil || group < 0 {
		return 0, false, fmt.Errorf("invalid stat process group %.32q", fields[2])
	}
	threads, err := strconv.Atoi(fields[17])
	if err != nil || threads < 0 {
		return 0, false, fmt.Errorf("invalid stat thread count")
	}
	// A zombie leader may still have live threads. Treat it as stopped only
	// after no sibling thread remains; live/stopped/D-state tasks are unconfirmed.
	terminal := fields[0] == "Z" || fields[0] == "X" || fields[0] == "x"
	return group, terminal && threads <= 1, nil
}
