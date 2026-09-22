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
	"fmt"
	"runtime"
	"strconv"

	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
	"golang.org/x/sys/unix"
)

const (
	QemuVmm    VmmType = "qemu"
	QemuBinary string  = "qemu-system-"
)

type Qemu struct {
	binaryPath string
	binary     string
	vhost      bool
}

func (q *Qemu) Signal(pid int, signal unix.Signal) error {
	return signalProcess(pid, signal)
}

func (q *Qemu) Stop(pid int) error {
	return killProcess(pid)
}

func (q *Qemu) Ok() error {
	return nil
}

// UsesKVM returns a bool value depending on if the monitor uses KVM
func (q *Qemu) UsesKVM() bool {
	return true
}

// SupportsSharedfs returns a bool value depending on the monitor support for shared-fs
func (q *Qemu) SupportsSharedfs(_ string) bool {
	return true
}

// SupportsControlSocket reports that QEMU exposes a control socket (QMP).
func (q *Qemu) SupportsControlSocket() bool {
	return true
}

func (q *Qemu) Path() string {
	return q.binaryPath
}

func (q *Qemu) BuildExecCmd(args types.ExecArgs, ukernel types.Unikernel) ([]string, error) {
	qemuMem := BytesToStringMB(args.MemSizeB)
	exArgs := []string{
		q.binaryPath,
		"-m", qemuMem + "M",
		"-L", "/usr/share/qemu", // Set the path for qemu bios/data
		"-cpu", "host", // Choose CPU
		"-enable-kvm",      // Enable KVM to use CPU virt extensions
		"-display", "none", // Disable graphic output
		"-vga", "none",
		"-serial", "stdio",
		"-monitor", "null",
	}
	// Expose the QMP socket only when socket_path is set. server=on,wait=off lets
	// QEMU boot without waiting for a client.
	if args.SocketPath != "" {
		exArgs = append(exArgs, "-qmp", "unix:"+args.SocketPath+",server=on,wait=off")
	}

	if args.VCPUs > 0 {
		exArgs = append(exArgs, "-smp", strconv.FormatUint(uint64(args.VCPUs), 10))
	}

	if args.Seccomp {
		// Enable Seccomp in QEMU. The sandbox options below:
		//   obsolete=deny          - Deny Obsolete system calls
		//   elevateprivileges=deny - Deny set*uid|gid system calls
		//   spawn=deny             - Deny *fork and execve
		//   resourcecontrol=deny   - Deny process affinity and scheduler priority
		exArgs = append(exArgs,
			"--sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny",
		)
	}

	machineArgs, err := q.machineArgs(args, qemuMem)
	if err != nil {
		return nil, err
	}
	exArgs = append(exArgs, machineArgs...)

	if args.FirmwarePath != "" {
		exArgs = append(exArgs, "-bios", args.FirmwarePath)
	}

	exArgs = append(exArgs, "-kernel", args.UnikernelPath)
	if args.Net.TapDev != "" {
		netcli := ukernel.MonitorNetCli(args.Net.TapDev, args.Net.MAC)
		if len(netcli) == 0 {
			netdevArg := fmt.Sprintf("tap,id=net0,script=no,downscript=no,ifname=%s", args.Net.TapDev)
			if q.vhost {
				netdevArg += ",vhost=on"
			}
			exArgs = append(exArgs, "-netdev", netdevArg)

			devType := virtioDevice("virtio-net", args)
			netCliDev := fmt.Sprintf("%s,netdev=net0,host_mtu=%d,mac=%s%s", devType, args.Net.MTU, args.Net.MAC, virtioTEEOptions(args))
			exArgs = append(exArgs, "-device", netCliDev)
		} else {
			exArgs = append(exArgs, netcli...)
		}
	} else {
		exArgs = append(exArgs, "-nic", "none")
	}

	blockArgs := ukernel.MonitorBlockCli()
	for _, blockArg := range blockArgs {
		if blockArg.ID != "" && blockArg.Path != "" {
			devArg := fmt.Sprintf("%s,serial=%s,drive=%s%s", virtioDevice("virtio-blk", args), blockArg.ID, blockArg.ID, virtioTEEOptions(args))
			if !usesVirtioMMIO(args) {
				devArg += ",scsi=off"
			}
			drvArg := fmt.Sprintf("format=raw,if=none,id=%s,file=%s", blockArg.ID, blockArg.Path)
			exArgs = append(exArgs, "-device", devArg, "-drive", drvArg)
		} else if len(blockArg.ExactArgs) > 0 {
			exArgs = append(exArgs, blockArg.ExactArgs...)
		}
	}

	if args.InitrdPath != "" {
		exArgs = append(exArgs, "-initrd", args.InitrdPath)
	}

	switch args.Sharedfs.Type {
	case "9pfs":
		// The 9p transport depends on the guest, so let the guest supply
		// the cli options; nil means no support for 9pfs
		sharedFSOption := ukernel.MonitorSharedfsCli(args.Sharedfs.Type, args.Sharedfs.Path)
		exArgs = append(exArgs, sharedFSOption...)
	case "virtiofs":
		if args.TEE != types.TEENone {
			return nil, fmt.Errorf("virtiofs is not supported for confidential guests")
		}
		objArg := fmt.Sprintf("memory-backend-file,id=mem,size=%sM,mem-path=/tmp,share=on", qemuMem)
		exArgs = append(exArgs,
			"-object", objArg,
			"-numa", "node,memdev=mem",
			"-chardev", "socket,id=char0,path=/tmp/vhostqemu",
			"-device", "vhost-user-fs-pci,queue-size=1024,chardev=char0,tag=fs0",
		)
	default:
		// Nothing to add
	}

	extraMonArgs := ukernel.MonitorCli()
	if extraMonArgs.ExtraInitrd != "" {
		exArgs = append(exArgs, "-initrd", extraMonArgs.ExtraInitrd)
	}
	exArgs = append(exArgs, extraMonArgs.OtherArgs...)

	if args.VAccelType == "vsock" {
		vsockArg := fmt.Sprintf("vhost-vsock-pci,id=vhost-vsock-pci0,guest-cid=%d", args.VSockDevID)
		exArgs = append(exArgs, "-device", vsockArg)
	}

	exArgs = append(exArgs, "-append", args.Command)
	return exArgs, nil
}

// PreExec performs pre-execution setup. QEMU has no special pre-exec requirements.
func (q *Qemu) PreExec(_ types.ExecArgs) error {
	return nil
}

// usesVirtioMMIO reports whether the selected machine exposes virtio devices
// over MMIO (microvm on x86, virt on arm64) instead of PCI.
func usesVirtioMMIO(args types.ExecArgs) bool {
	if runtime.GOARCH == "arm64" {
		return true
	}
	return args.Machine == types.MachineMicrovm
}

// virtioDevice returns the QEMU device name of a virtio device for the
// selected machine's transport, e.g. virtio-net-pci or virtio-net-device.
func virtioDevice(base string, args types.ExecArgs) string {
	if usesVirtioMMIO(args) {
		return base + "-device"
	}
	return base + "-pci"
}

// virtioTEEOptions returns the extra device options a confidential guest
// requires: the guest kernel refuses virtio devices that do not advertise
// VIRTIO_F_ACCESS_PLATFORM (iommu_platform) and, on PCI, legacy devices.
func virtioTEEOptions(args types.ExecArgs) string {
	if args.TEE == types.TEENone {
		return ""
	}
	if usesVirtioMMIO(args) {
		return ",iommu_platform=on"
	}
	return ",iommu_platform=on,disable-legacy=on"
}

// machineArgs builds the -M (machine) and, for confidential guests, the
// -object arguments. On x86 the default machine (pc) is left implicit, so
// the command line of existing deployments does not change.
func (q *Qemu) machineArgs(args types.ExecArgs, memMB string) ([]string, error) {
	var machine string
	switch {
	case runtime.GOARCH == "arm64":
		// TODO: explore alternative implementations
		machine = "virt"
	case args.Machine == "" || args.Machine == types.MachinePC:
		machine = ""
	case args.Machine == types.MachineQ35:
		machine = types.MachineQ35
	case args.Machine == types.MachineMicrovm:
		// ACPI is required for the guest to find the virtio-mmio devices
		// and to shut down cleanly.
		machine = "microvm,acpi=on"
	default:
		return nil, fmt.Errorf("unsupported qemu machine type %q", args.Machine)
	}

	var exArgs []string
	switch args.TEE {
	case types.TEENone:
		if machine != "" {
			exArgs = append(exArgs, "-M", machine)
		}
	case types.TEESevSnp:
		if runtime.GOARCH != "amd64" {
			return nil, fmt.Errorf("sev-snp guests are only supported on x86_64")
		}
		if args.FirmwarePath == "" {
			return nil, fmt.Errorf("sev-snp guests require a firmware (firmware_path)")
		}
		if machine == "" {
			machine = types.MachinePC
		}
		// kernel-hashes=on puts the hashes of the kernel, the initrd and the
		// command line in the launch measurement, so a firmware that honors
		// the hashes table (OVMF AmdSev, Oak stage0 with sev_kernel_hashes)
		// verifies the fw_cfg-provided boot payload.
		exArgs = append(exArgs,
			"-M", machine+",confidential-guest-support=sev0,memory-backend=ram1",
			"-object", "sev-snp-guest,id=sev0,cbitpos=51,reduced-phys-bits=1,kernel-hashes=on",
			"-object", fmt.Sprintf("memory-backend-memfd,id=ram1,size=%sM,share=true,prealloc=false", memMB),
		)
	case types.TEETdx:
		if runtime.GOARCH != "amd64" {
			return nil, fmt.Errorf("tdx guests are only supported on x86_64")
		}
		if args.FirmwarePath == "" {
			return nil, fmt.Errorf("tdx guests require a firmware (firmware_path)")
		}
		if machine != types.MachineQ35 {
			return nil, fmt.Errorf("tdx guests require the q35 machine type")
		}
		exArgs = append(exArgs,
			"-M", machine+",kernel-irqchip=split,confidential-guest-support=tdx0",
			"-object", "tdx-guest,id=tdx0",
		)
	default:
		return nil, fmt.Errorf("unsupported tee type %q", args.TEE)
	}

	return exArgs, nil
}
