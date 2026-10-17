//go:build linux
// +build linux

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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

const (
	testHviKernel = "/rootfs/bzImage"
	testRootfsImg = "/run/urunc/rootfs.img"
	testDataImg   = "/run/urunc/data.img"
)

// flagValue returns the value following the first occurrence of flag, and
// whether the flag was present at all.
func flagValue(argv []string, flag string) (string, bool) {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1], true
		}
	}
	return "", false
}

// countFlag returns how many times flag appears in argv.
func countFlag(argv []string, flag string) int {
	n := 0
	for _, a := range argv {
		if a == flag {
			n++
		}
	}
	return n
}

// TestHviBuildExecCmdDisk covers how the single --disk slot is filled. hvi
// enumerates virtio-mmio devices in a fixed order, so whichever image lands
// here becomes the guest's /dev/vda -- which the Linux boot line names as
// root= when the rootfs type is "block".
func TestHviBuildExecCmdDisk(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		blockCli     []types.MonitorBlockArgs
		blockDevPath string
		wantDisk     string
		wantNoDisk   bool
		wantErr      bool
	}{
		{
			// The regression this whole change exists for: with mountRootfs=true
			// the rootfs arrives through MonitorBlockCli, and if it never reaches
			// hvi the guest boots with root=/dev/vda and no vda behind it.
			name:     "rootfs image becomes the disk",
			blockCli: []types.MonitorBlockArgs{{ID: "rootfs", Path: testRootfsImg}},
			wantDisk: testRootfsImg,
		},
		{
			name:     "a lone volume is used when there is no rootfs image",
			blockCli: []types.MonitorBlockArgs{{ID: "data", Path: testDataImg}},
			wantDisk: testDataImg,
		},
		{
			// hvi has one disk slot. Booting without a volume the container
			// asked for is a silent data fault, so this is an error rather than
			// a dropped device -- in either order.
			name: "more devices than slots is refused",
			blockCli: []types.MonitorBlockArgs{
				{ID: "rootfs", Path: testRootfsImg},
				{ID: "data", Path: testDataImg},
			},
			wantErr: true,
		},
		{
			name: "more devices than slots is refused, rootfs last",
			blockCli: []types.MonitorBlockArgs{
				{ID: "data", Path: testDataImg},
				{ID: "rootfs", Path: testRootfsImg},
			},
			wantErr: true,
		},
		{
			// A block CLI entry takes precedence over the darwin-only
			// BlockDevPath; they must not both land on the command line.
			name:         "block CLI wins over BlockDevPath",
			blockCli:     []types.MonitorBlockArgs{{ID: "rootfs", Path: testRootfsImg}},
			blockDevPath: testDataImg,
			wantDisk:     testRootfsImg,
		},
		{
			name:         "BlockDevPath is the fallback when there is no block CLI",
			blockDevPath: testRootfsImg,
			wantDisk:     testRootfsImg,
		},
		{
			// Entries with no host path are not devices; they must not consume
			// the slot and must not be counted towards the limit.
			name:     "entries without a path are ignored",
			blockCli: []types.MonitorBlockArgs{{ID: "rootfs"}, {ID: "data", Path: testDataImg}},
			wantDisk: testDataImg,
		},
		{
			name:       "an initrd-only boot passes no disk",
			wantNoDisk: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			hvi := &Hvi{binaryPath: "/usr/local/bin/hvi", binary: HviBinary}
			argv, err := hvi.BuildExecCmd(types.ExecArgs{
				KernelPath:   testHviKernel,
				BlockDevPath: tc.blockDevPath,
			}, &fakeUnikernel{blockCli: tc.blockCli})

			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			got, present := flagValue(argv, "--disk")
			if tc.wantNoDisk {
				assert.False(t, present, "expected no --disk in %v", argv)
				return
			}
			assert.True(t, present, "expected --disk in %v", argv)
			assert.Equal(t, tc.wantDisk, got)
			// hvi's parser keeps the last --disk it sees, so a second one would
			// silently override the image we chose.
			assert.Equal(t, 1, countFlag(argv, "--disk"), "argv: %v", argv)
		})
	}
}

// TestHviBuildExecCmdIntrospectSock checks the one piece of wiring that lets
// the out-of-VMM sidecar attach to hvi unchanged: urunc signals the opt-in by
// putting the control-socket path in the monitor's environment, and hvi takes
// it as a flag.
func TestHviBuildExecCmdIntrospectSock(t *testing.T) {
	t.Parallel()

	hvi := &Hvi{binaryPath: "/usr/local/bin/hvi", binary: HviBinary}
	argv, err := hvi.BuildExecCmd(types.ExecArgs{
		KernelPath:  testHviKernel,
		Environment: []string{"PATH=/usr/bin", telemControlSockEnv + "=/urunc-telem.sock"},
	}, &fakeUnikernel{})
	require.NoError(t, err)

	got, present := flagValue(argv, "--introspect-sock")
	assert.True(t, present, "expected --introspect-sock in %v", argv)
	assert.Equal(t, "/urunc-telem.sock", got)
}

// A virtiofs rootfs is served by the virtiofsd that preStartCmd starts. hvi
// gets the daemon's socket under the tag the boot line mounts as root, and
// nothing else about the export.
func TestHviBuildExecCmdVirtiofs(t *testing.T) {
	t.Parallel()

	hvi := &Hvi{binaryPath: "/usr/local/bin/hvi", binary: HviBinary}
	assert.True(t, hvi.SupportsSharedfs("virtio"))
	assert.False(t, hvi.SupportsSharedfs("9p"))

	argv, err := hvi.BuildExecCmd(types.ExecArgs{
		KernelPath: testHviKernel,
		Sharedfs:   types.SharedfsParams{Type: "virtiofs", Path: "/rootfs"},
	}, &fakeUnikernel{})
	require.NoError(t, err)
	got, present := flagValue(argv, "--fs")
	assert.True(t, present, "expected --fs in %v", argv)
	assert.Equal(t, "tag=fs0,socket=/tmp/vhostqemu", got)
	assert.Equal(t, 1, countFlag(argv, "--fs"), "argv: %v", argv)
	assert.Equal(t, 0, countFlag(argv, "--disk"), "a virtiofs root needs no disk: %v", argv)

	argv, err = hvi.BuildExecCmd(types.ExecArgs{KernelPath: testHviKernel}, &fakeUnikernel{})
	require.NoError(t, err)
	assert.Equal(t, 0, countFlag(argv, "--fs"), "argv: %v", argv)
}

// The guest attaches to the container's tap with its MAC. hvi's built-in
// stack (--net-stub, once --net) forwards no traffic.
func TestHviBuildExecCmdTap(t *testing.T) {
	t.Parallel()

	hvi := &Hvi{binaryPath: "/usr/local/bin/hvi", binary: HviBinary}
	argv, err := hvi.BuildExecCmd(types.ExecArgs{
		KernelPath: testHviKernel,
		Net:        types.NetDevParams{TapDev: "tap0_urunc", MAC: "02:00:00:00:00:01"},
	}, &fakeUnikernel{})
	require.NoError(t, err)
	tap, present := flagValue(argv, "--net-tap")
	assert.True(t, present, "expected --net-tap in %v", argv)
	assert.Equal(t, "tap0_urunc", tap)
	mac, present := flagValue(argv, "--net-mac")
	assert.True(t, present, "expected --net-mac in %v", argv)
	assert.Equal(t, "02:00:00:00:00:01", mac)
	assert.Equal(t, 0, countFlag(argv, "--net"), "argv: %v", argv)
	assert.Equal(t, 0, countFlag(argv, "--net-stub"), "argv: %v", argv)

	argv, err = hvi.BuildExecCmd(types.ExecArgs{KernelPath: testHviKernel}, &fakeUnikernel{})
	require.NoError(t, err)
	assert.Equal(t, 0, countFlag(argv, "--net-tap"), "argv: %v", argv)
}

// TestHviBuildExecCmdNeedsKernel guards the one hard requirement: hvi has no
// default kernel, so an image with neither a kernel nor a unikernel path must
// fail here rather than at exec time.
func TestHviBuildExecCmdNeedsKernel(t *testing.T) {
	t.Parallel()

	hvi := &Hvi{binaryPath: "/usr/local/bin/hvi", binary: HviBinary}
	_, err := hvi.BuildExecCmd(types.ExecArgs{}, &fakeUnikernel{})
	require.Error(t, err)
}
