//go:build darwin

package hypervisors

import (
	"strings"
	"testing"

	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

func TestHviDarwinGenericContainerBoot(t *testing.T) {
	hvi := NewHviDarwin("/opt/hvi")
	args := types.ExecArgs{
		ContainerID:   "alpine-test",
		KernelPath:    "/host/Image",
		InitrdPath:    "/instance/container-initrd",
		Command:       "rdinit=/vz-init console=ttyAMA0",
		MemSizeB:      512 << 20,
		VCPUs:         2,
		AgentSockPath: "/instance/agent.sock",
		Net:           types.NetDevParams{TapDev: "en0"},
		Sharedfs: types.SharedfsParams{
			Type: "virtiofs", Path: "/store/alpine/rootfs", Tag: "rootfs", ReadOnly: true,
		},
		SharedDirs: []types.SharedDirParams{
			{Path: "/host/config", Tag: "share0", ReadOnly: true},
			{Path: "/host/models", Tag: "share1"},
		},
	}
	argv, err := hvi.BuildExecCmd(args, &fakeUnikernel{})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ") + " "
	for _, want := range []string{
		"/opt/hvi boot",
		"--kernel /host/Image",
		"--initramfs /instance/container-initrd",
		"--fs tag=rootfs,path=/store/alpine/rootfs,readonly=on",
		"--fs tag=share0,path=/host/config,readonly=on",
		"--fs tag=share1,path=/host/models ",
		"--cmdline rdinit=/vz-init console=ttyAMA0",
		"--agent-sock /instance/agent.sock",
		"--net",
		"--sandbox-id alpine-test",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q in HVI command:\n%s", want, joined)
		}
	}
}

func TestHviDarwinWritableAndDuplicateShares(t *testing.T) {
	hvi := NewHviDarwin("/opt/hvi")
	argv, err := hvi.BuildExecCmd(types.ExecArgs{
		KernelPath: "/host/Image",
		Sharedfs:   types.SharedfsParams{Path: "/host/rootfs"},
	}, &fakeUnikernel{})
	if err != nil {
		t.Fatalf("writable export: %v", err)
	}
	if got := strings.Join(argv, " ") + " "; !strings.Contains(got, "--fs tag=rootfs,path=/host/rootfs ") {
		t.Fatalf("writable root export missing from %s", got)
	}
	argv, err = hvi.BuildExecCmd(types.ExecArgs{
		KernelPath: "/host/Image",
		SharedDirs: []types.SharedDirParams{{Path: "/host/extra", Tag: "extra"}},
	}, &fakeUnikernel{})
	if err != nil {
		t.Fatalf("writable additional export: %v", err)
	}
	if got := strings.Join(argv, " ") + " "; !strings.Contains(got, "--fs tag=extra,path=/host/extra ") {
		t.Fatalf("writable additional export missing from %s", got)
	}
	_, err = hvi.BuildExecCmd(types.ExecArgs{
		KernelPath: "/host/Image",
		Sharedfs:   types.SharedfsParams{Path: "/host/rootfs", Tag: "same", ReadOnly: true},
		SharedDirs: []types.SharedDirParams{{Path: "/host/extra", Tag: "same", ReadOnly: true}},
	}, &fakeUnikernel{})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate tag: got %v", err)
	}
}

// A workload running as an unprivileged user gets each export owned by it,
// on the export itself. The process-wide --fs-uid/--fs-gid pair is
// deprecated, and hvi prints a note for each use.
func TestHviDarwinExportOwner(t *testing.T) {
	hvi := NewHviDarwin("/opt/hvi")
	argv, err := hvi.BuildExecCmd(types.ExecArgs{
		KernelPath: "/host/Image",
		GuestUID:   1000,
		GuestGID:   1000,
		Sharedfs:   types.SharedfsParams{Path: "/host/rootfs", Tag: "rootfs", ReadOnly: true},
		SharedDirs: []types.SharedDirParams{{Path: "/host/work", Tag: "work"}},
	}, &fakeUnikernel{})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(argv, " ") + " "
	for _, want := range []string{
		"--fs tag=rootfs,path=/host/rootfs,readonly=on,uid=1000,gid=1000 ",
		"--fs tag=work,path=/host/work,uid=1000,gid=1000 ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in %s", want, got)
		}
	}
	for _, old := range []string{"--fs-uid", "--fs-gid", "--share-ro", "--share-rw"} {
		if strings.Contains(got, old) {
			t.Errorf("deprecated flag %s in %s", old, got)
		}
	}

	// root is hvi's default owner, so it is not spelled out.
	argv, err = hvi.BuildExecCmd(types.ExecArgs{
		KernelPath: "/host/Image",
		Sharedfs:   types.SharedfsParams{Path: "/host/rootfs", Tag: "rootfs"},
	}, &fakeUnikernel{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(argv, " "); strings.Contains(got, "uid=") || strings.Contains(got, "gid=") {
		t.Fatalf("root owner should be left to hvi's default: %s", got)
	}
}

// --fs is split on commas with no escape, so a comma in a path or tag would
// be parsed as the next option.
func TestHviDarwinExportRefusesComma(t *testing.T) {
	hvi := NewHviDarwin("/opt/hvi")
	for _, share := range []types.SharedfsParams{
		{Path: "/host/a,b", Tag: "rootfs"},
		{Path: "/host/rootfs", Tag: "a,b"},
	} {
		_, err := hvi.BuildExecCmd(types.ExecArgs{KernelPath: "/host/Image", Sharedfs: share}, &fakeUnikernel{})
		if err == nil || !strings.Contains(err.Error(), "comma") {
			t.Errorf("%+v: got %v, want a comma refusal", share, err)
		}
	}
}

func TestHviDarwinGatewayNetwork(t *testing.T) {
	hvi := NewHviDarwin("/opt/hvi")
	argv, err := hvi.BuildExecCmd(types.ExecArgs{
		KernelPath: "/host/Image",
		Net: types.NetDevParams{
			TapDev:     "en0",
			UnixSocket: "/run/hull/gateway.qemu",
		},
	}, &fakeUnikernel{})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(argv, " ")
	if !strings.Contains(got, "--net-gateway /run/hull/gateway.qemu") {
		t.Fatalf("gateway network missing from %s", got)
	}
	if strings.Contains(got, " --net ") || strings.HasSuffix(got, " --net") {
		t.Fatalf("built-in network must not accompany gateway network: %s", got)
	}
}
