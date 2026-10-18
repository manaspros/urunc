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
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"
)

func TestBytesToMiB(t *testing.T) {
	t.Parallel()

	const mib uint64 = 1024 * 1024

	cases := []struct {
		name     string
		input    uint64
		expected uint64
	}{
		{"zero", 0, 0},
		{"less than one MiB truncates to zero", mib - 1, 0},
		{"exactly one MiB", mib, 1},
		{"exactly two MiB", 2 * mib, 2},
		{"non-multiple truncates down", mib + (mib / 2), 1},
		{"large value", 1024 * mib, 1024},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.expected, bytesToMiB(tc.input))
		})
	}
}

func TestBytesToMB(t *testing.T) {
	t.Parallel()

	const mb uint64 = 1000 * 1000

	cases := []struct {
		name     string
		input    uint64
		expected uint64
	}{
		{"zero", 0, 0},
		{"less than one MB truncates to zero", mb - 1, 0},
		{"exactly one MB", mb, 1},
		{"exactly two MB", 2 * mb, 2},
		{"non-multiple truncates down", mb + (mb / 2), 1},
		{"large value", 1024 * mb, 1024},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.expected, bytesToMB(tc.input))
		})
	}
}

// stubSysKill replaces sysKill with a recorder for the duration of the test.
// Tests using it must not call t.Parallel.
func stubSysKill(t *testing.T) *[][2]int {
	t.Helper()
	calls := &[][2]int{}
	orig := sysKill
	sysKill = func(pid int, sig unix.Signal) error {
		*calls = append(*calls, [2]int{pid, int(sig)})
		return nil
	}
	t.Cleanup(func() { sysKill = orig })
	return calls
}

func TestSignalProcessRefusesInvalidPid(t *testing.T) {
	for _, pid := range []int{-1, 0} {
		calls := stubSysKill(t)
		err := signalProcess(pid, unix.SIGKILL)
		assert.Error(t, err, "pid %d", pid)
		assert.Empty(t, *calls, "pid %d", pid)
	}
}

func TestKillProcessRefusesInvalidPid(t *testing.T) {
	for _, pid := range []int{-1, 0} {
		calls := stubSysKill(t)
		err := killProcess(pid)
		assert.Error(t, err, "pid %d", pid)
		assert.Empty(t, *calls, "pid %d", pid)
	}
}

func TestVMMSignalStopRefuseInvalidPid(t *testing.T) {
	vmms := map[string]interface {
		Signal(int, unix.Signal) error
		Stop(int) error
	}{
		"cloud-hypervisor": &CloudHypervisor{},
		"qemu":             &Qemu{},
		"firecracker":      &Firecracker{},
		"hvt":              &HVT{},
		"spt":              &SPT{},
		"hyperlight":       &Hyperlight{},
	}
	for name, vmm := range vmms {
		for _, pid := range []int{-1, 0} {
			calls := stubSysKill(t)
			assert.Error(t, vmm.Signal(pid, unix.SIGKILL), "%s Signal(%d, SIGKILL)", name, pid)
			assert.Error(t, vmm.Signal(pid, unix.SIGTERM), "%s Signal(%d, SIGTERM)", name, pid)
			assert.Error(t, vmm.Stop(pid), "%s Stop(%d)", name, pid)
			assert.Empty(t, *calls, "%s pid %d", name, pid)
		}
	}
}

func TestSignalProcessReachesSysKill(t *testing.T) {
	calls := stubSysKill(t)
	pid := os.Getpid()
	assert.NoError(t, signalProcess(pid, 0))
	assert.Equal(t, [][2]int{{pid, 0}}, *calls)
}

func startSleeper(t testing.TB) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "1000")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start sleep: %v", err)
	}
	return cmd
}

func TestKillProcessKillsRunningProcess(t *testing.T) {
	cmd := startSleeper(t)
	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()

	assert.NoError(t, killProcess(cmd.Process.Pid))
	select {
	case <-reaped:
	case <-time.After(5 * time.Second):
		t.Fatal("process was not killed")
	}
}

// A killed process stays a zombie until its parent reaps it. That must not
// make killProcess wait for the whole timeout.
func TestKillProcessDoesNotWaitForReaping(t *testing.T) {
	cmd := startSleeper(t)
	t.Cleanup(func() { _ = cmd.Wait() })

	start := time.Now()
	assert.NoError(t, killProcess(cmd.Process.Pid))
	assert.Less(t, time.Since(start), time.Second)
}

func TestKillProcessAlreadyDead(t *testing.T) {
	cmd := startSleeper(t)
	pid := cmd.Process.Pid
	assert.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()

	assert.NoError(t, killProcess(pid))
}

// BenchmarkKillProcess kills a real child that another goroutine reaps, the
// way the containerd shim reaps the monitor.
func BenchmarkKillProcess(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		cmd := startSleeper(b)
		go func() { _ = cmd.Wait() }()
		b.StartTimer()
		if err := killProcess(cmd.Process.Pid); err != nil {
			b.Fatal(err)
		}
	}
}
