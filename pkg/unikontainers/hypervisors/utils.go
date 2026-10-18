// Copyright (c) 2023-2026, Nubificus LTD
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hypervisors

import (
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

func cpuArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "aarch64"
	case "amd64":
		return "x86_64"
	default:
		return ""
	}
}

func bytesToMiB(bytes uint64) uint64 {
	const bytesInMiB = 1024 * 1024
	return bytes / bytesInMiB
}

func bytesToMB(bytes uint64) uint64 {
	const bytesInMB = 1000 * 1000
	return bytes / bytesInMB
}

func BytesToStringMB(argMem uint64) string {
	stringMem := strconv.FormatUint(DefaultMemory, 10)
	if argMem != 0 {
		userMem := bytesToMB(argMem)
		// Check for too low memory
		if userMem == 0 {
			userMem = DefaultMemory
		}
		stringMem = strconv.FormatUint(userMem, 10)
	}

	return stringMem
}

// sysKill is the kill(2) used by this package. Tests replace it so that no
// real signal is ever sent.
var sysKill = unix.Kill

// signalProcess sends sig to pid. It refuses pids <= 0: kill(2) treats them
// as process groups (0 is the caller's group, -1 is every process the caller
// may signal), never as a single monitor process.
func signalProcess(pid int, sig unix.Signal) error {
	if pid <= 0 {
		return fmt.Errorf("refusing to signal invalid pid %d", pid)
	}
	return sysKill(pid, sig)
}

func killProcess(pid int) error {
	const timeout = 2 * time.Second
	if pid <= 0 {
		return fmt.Errorf("refusing to signal invalid pid %d", pid)
	}
	// Get a handle on the process before killing it. This way we can block
	// until it exits instead of polling. pidfd_open(2) needs Linux 5.3, so
	// when it fails for any reason other than a dead process, we fall back
	// to polling.
	pidFd, pidFdErr := unix.PidfdOpen(pid, 0)
	if pidFdErr == nil {
		defer unix.Close(pidFd)
	} else if errors.Is(pidFdErr, unix.ESRCH) {
		// Process already dead, nothing to do
		return nil
	}
	err := sysKill(pid, unix.SIGKILL)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			// Process already dead, nothing to do
			return nil
		}
		return err
	}
	deadline := time.Now().Add(timeout)
	if pidFdErr == nil {
		return waitPidFd(pidFd, pid, deadline)
	}
	for {
		if err := sysKill(pid, 0); err != nil {
			if errors.Is(err, unix.ESRCH) {
				// process is dead
				break
			}
			return fmt.Errorf("error checking if process with pid %d is alive: %w", pid, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for pid %d to die", pid)
		}
		time.Sleep(100 * time.Millisecond)
	}

	return nil
}

// waitPidFd blocks until the process behind pidFd exits or the deadline
// passes. A pidfd becomes readable as soon as the process exits, so a zombie
// that nobody has reaped yet counts as dead too.
func waitPidFd(pidFd int, pid int, deadline time.Time) error {
	pfds := []unix.PollFd{{Fd: int32(pidFd), Events: unix.POLLIN}} //nolint:gosec // a file descriptor always fits in an int32
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("timeout waiting for pid %d to die", pid)
		}
		// Round up, so that we never poll for 0 ms when time is left.
		n, err := unix.Poll(pfds, int(remaining.Milliseconds())+1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("error waiting for pid %d to die: %w", pid, err)
		}
		if n > 0 {
			return nil
		}
	}
}
