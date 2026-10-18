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

package unikontainers

import (
	"fmt"
	"strings"
	"testing"

	"github.com/moby/sys/mountinfo"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

func TestGetBlockDevice(t *testing.T) {
	// Create a mock partition
	tmpMnt := types.BlockDevParams{
		Source:     "proc",
		MountPoint: "/proc",
		FsType:     "proc",
		ID:         "",
	}

	rootFs, err := getMountInfo("/proc")
	assert.NoError(t, err, "Expected no error in getting block device")
	assert.Equal(t, tmpMnt.Source, rootFs.Source, "Incorrect image")
	assert.Equal(t, tmpMnt.MountPoint, rootFs.MountPoint, "Incorrect mountpoint")
	assert.Equal(t, tmpMnt.FsType, rootFs.FsType, "Expected filesystem type to be proc")
	assert.Equal(t, tmpMnt.ID, rootFs.ID, "Expected ID to be empty")
}

// TestFindMountInfoEscapedPath reproduces a bind mount whose source path
// contains a space, a tab or a backslash. The kernel octal-escapes these
// characters in mountinfo (e.g. a space becomes \040), so this makes sure
// that findMountInfo still matches the real, unescaped path against the
// parsed mountinfo entry.
func TestFindMountInfoEscapedPath(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		escapedRaw string
	}{
		{name: "space", path: "/mnt/my volume", escapedRaw: `/mnt/my\040volume`},
		{name: "tab", path: "/mnt/my\tvolume", escapedRaw: `/mnt/my\011volume`},
		{name: "backslash", path: `/mnt/my\volume`, escapedRaw: `/mnt/my\134volume`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line := "36 35 8:1 / " + tt.escapedRaw + " rw,relatime shared:1 - ext4 /dev/sdb1 rw"
			mounts, err := mountinfo.GetMountsFromReader(strings.NewReader(line), nil)
			assert.NoError(t, err, "expected the synthetic mountinfo line to parse")

			blockDev, err := findMountInfo(mounts, tt.path)
			assert.NoError(t, err, "expected the escaped mount point to match the real path")
			assert.Equal(t, "/dev/sdb1", blockDev.Source)
			assert.Equal(t, "ext4", blockDev.FsType)
			assert.Equal(t, tt.path, blockDev.MountPoint)
		})
	}
}

// BenchmarkGetBlockVolumes measures getBlockVolumes for a container with n
// bind mounts. None of the sources is a mount point, so nothing is unmounted
// and no privileges are needed. This is the common case, e.g. the
// /etc/hosts, /etc/resolv.conf and termination-log binds of a Pod.
func BenchmarkGetBlockVolumes(b *testing.B) {
	for _, n := range []int{1, 8, 16} {
		mounts := make([]specs.Mount, 0, n)
		for i := 0; i < n; i++ {
			mounts = append(mounts, specs.Mount{
				Destination: fmt.Sprintf("/vol%d", i),
				Type:        "bind",
				Source:      fmt.Sprintf("/nonexistent/urunc-bench/vol%d", i),
			})
		}
		b.Run(fmt.Sprintf("binds=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				vols, err := getBlockVolumes(mounts, nil)
				if err != nil || len(vols) != 0 {
					b.Fatalf("unexpected result: %v, %v", vols, err)
				}
			}
		})
	}
}
