# Confidential containers with urunc: feasibility study

Date: 2026-10-10. Audience: `urunc` maintainers and the NOFire AI team.

This is a desk study without Intel TDX or AMD SEV-SNP hardware. Nothing
confidential was run. Every number carries one of three tags:

- **[M]** measured, as reported in the cited source;
- **[D]** derived arithmetically from a cited measurement;
- **[E]** our estimate, with the reasoning stated next to it.

Citations are footnotes, such as [^3]; the full list is in
[section 11](#11-references).

## 1. Summary

- **Feasible, in three stages:** SEV-SNP on QEMU, then TDX, then a unikernel guest.
- Stage 1 needs no new guest: the single-process Linux guest with `urunit`.
- Gains: a tiny measured firmware with hash-verified direct boot, no agent or in-guest pull, small RAM.
- **3-10x faster than a Kata-CVM VM boot** (1.8-6.3 s [M] [^4] to 0.35-0.8 s [E] on SNP).
- **10-20x faster than a CoCo pod start** [E] (wider scope); about equal to a SEVeriFast micro-CVM [^4].
- A unikernel adds only **1.3-2x** on latency [E]; its value is memory and TCB.
- **Memory density 4-12x** over a CoCo pod for small images [E].
- SNP burst launches are serialized by the AMD PSP; TDX has no such bottleneck.

## 2. Background: what urunc does today that matters

`urunc` replaces the container process with a VMM process that boots a
single-application guest. The facts below are from HEAD `a1226ef`.

| Fact | Where | Consequence for confidential guests |
|---|---|---|
| The reexec process `execve`s the VMM, so the VMM *is* the container PID | `execMonitor`, `pkg/unikontainers/unikontainers.go:845` | No agent and no shim process. cgroups apply to the VMM directly. |
| The last timestamp is TS18 (`execve_hypervisor`). Nothing waits for guest boot. | `internal/metrics`, `script/performance/*.py`, `docs/developer-guide/timestamps.md` | A "guest ready" time must come from the guest console, not from `urunc`. |
| `MonitorConfig` has no field for firmware, machine type, platform flags or extra args | `pkg/unikontainers/types/types.go:145` | New fields are needed for `-bios`, `-M` and TEE objects. |
| QEMU on x86 boots the default `pc` machine with SeaBIOS | `pkg/unikontainers/hypervisors/qemu.go:68` | No firmware path today; CC needs q35 or microvm plus a CC firmware. |
| Cloud Hypervisor uses direct `--kernel`/`--initramfs` boot; Firecracker uses `/tmp/fc.json` | `pkg/unikontainers/hypervisors/cloud_hypervisor.go:73` | CH matches the v52 SNP direct-boot model; Firecracker has no CC path. |
| Guest/monitor pairs are hard-coded; Cloud Hypervisor runs Linux only | `pkg/unikontainers/annotations.go:76-92` | A separate TEE pair table is simple to add. |
| Rootfs: initrd (mutated in place: `urunit` config and bind mounts appended as cpio records), devmapper snapshot as `/dev/vda` read-write, explicit block image, 9p, virtiofs | `rootfs.go`, `block.go`, `initrd_rootfs.go`, `shared_fs.go`, `initrd/initrd.go` | Only the initrd is measurable as is. The per-launch mutation breaks reproducible digests. |
| Guest IP, env and cmd travel on the kernel cmdline or in the `urunit` config | `unikernels/linux.go`, `unikernels/freebsd.go` | The cmdline is host-visible and, in a CVM, measured. |
| `ExecArgs` is built only in `buildMonitorSpec` and saved to `.monitor_spec.json`; `MonitorConfig` round-trips through `Map()`/`UruncConfigFromMap()` | `unikontainers.go:491-563`, `urunc_config.go` | New fields must be added in three places or they are lost between `create` and `start`. |

There is no confidential-computing code, documentation, test or CI hardware
in `urunc`. `urunc-deploy` already ships Kata's QEMU build from kata-deploy
3.15.0 [^33]. `urunc-deploy` and CI pin Cloud Hypervisor v50.0; SNP needs
v52 or later [^24].

The image format is the key property. A `urunc` image is a kernel plus an
initrd (or a unikernel binary). That is exactly what a measured direct boot
needs. CoCo instead boots a generic guest and pulls the container image
inside it [^1].

## 3. State of the ecosystem

### 3.1 VMMs

Status checked on 2026-10-10.

| VMM | SEV-SNP | TDX | CC direct boot | Note |
|---|---|---|---|---|
| QEMU >= 10.1 (Aug 2025); 11.0 adds CVM reset | upstream `sev-snp-guest`; IGVM (SNP only) [^21], [^47] | upstream `tdx-guest`, host Linux >= 6.16 [^22] | SNP: `-kernel` + `kernel-hashes=on`, or IGVM stage0. TDX: TDVF-metadata firmware via `-bios` is mandatory; `-kernel`/`-initrd` over fw_cfg, measured into RTMRs; GetQuote to QGS works | q35 documented; microvm unverified (stage0 claims SNP microvm support [^38]) |
| Cloud Hypervisor | KVM since v52.0 (2026-05-14) [^24]; measured kernel/initrd/cmdline hashes and SNP ID block [^25]; IGVM payload mandatory, kernel via fw_cfg; no hotplug, hugepages or virtio-iommu | bit-rotted [^26]; Intel PR #8724 closed unmerged in Aug 2026 in favour of a Cyberus Technology re-enablement [^27] | `--platform sev_snp=on --igvm stage0.igvm --kernel ... --initramfs ... --cmdline ...` | Best template for the `urunc` SNP path |
| libkrun | `libkrun-sev`, guest_memfd since v1.12 [^29] | `libkrun-tdx` since v1.15 (qboot: 1 vCPU, <= 3 GiB); td-shim on main since Aug-Sep 2026 (8 vCPU/16 GiB), unreleased [^29] | bundled firmware + patched kernel + initrd; SNP pre-encrypts the full payload; qboot TDX adds all low RAM (no lazy accept) | a library, not an exec'd binary; no published boot times |
| Firecracker | none | none | n/a | maintainers, 2025-06-05: "not on our roadmap" [^30]; research forks only [^3], [^43] |
| Hyperlight | none | none | n/a | issue #143 "Parked": snapshot/restore clashes with private memory [^31] |
| Solo5-hvt | none | none | n/a | [^32] |
| Kata/CoCo v0.23 (Sep 2026) | QEMU + OVMF `AMDSEV.fd`; host >= 6.11 | QEMU + TDVF | `-kernel` + initrd/dm-verity measured by firmware | no Firecracker or CH CC class on bare metal [^36] |

Host stack versions: KVM SNP host support landed in Linux 6.11, TDX host
support in 6.16 [^48]. QEMU gained `sev-snp-guest` in 9.1 and TDX plus IGVM in
10.1 [^21], [^22].

Corrections from our adversarial review, each checked against source:

- **td-shim does not boot on upstream QEMU.** QEMU's `hw/i386/tdvf.c` accepts
  only the BFV, CFV, TD_HOB and TEMP_MEM metadata sections [^23]. td-shim
  carries even a built-in payload in section type 5 (Payload) and has no
  fw_cfg loader [^37]. It works only with unreleased libkrun [^29], Intel's
  out-of-tree QEMU (used by Gramine-TDX [^7]), or a re-enabled CH TDX [^27].
  TDX on upstream QEMU means TDVF plus `-kernel`/`-initrd` over fw_cfg,
  measured into RTMR[^1].
- **Cloud Hypervisor SNP policy comes from the IGVM file.** Guest policy, ID
  block and ID auth are IGVM directives, not CLI flags. There is no
  host-data flag. The kernel/initrd/cmdline hash page is inserted only when
  both `--kernel` and `--cmdline` are given [^25]. A per-container
  `HOST_DATA` would need a per-launch IGVM rebuild. QEMU `sev-snp-guest` has
  `host-data=`, `id-block=` and `id-auth=` properties [^21]. PR #7942 defers
  configurable measured boot and policy injection from user parameters [^24].

### 3.2 Firmware options

TDX always starts at the fixed reset vector in 32-bit mode, so a TDX-aware
first stage is mandatory. On SNP the VMM supplies the initial VMSA, so a
guest can start in 64-bit mode without firmware [^3], [^38].

| Firmware | TEE | Size | Boot cost | Used by | Status |
|---|---|---|---|---|---|
| TDVF (OVMF IntelTdx) | TDX | ~4 MB; 764 KLoC [M] [^5] | 1.05 s in a TD [M] [^5]; pre-accepts ~512 MB [^34] | QEMU, Kata/CoCo | upstream [^39] |
| OVMF AmdSev | SNP | ~4 MB; 206 KLoC [M] [^5] | 3.81 s [M] [^5]; 3.2 s plus 288 ms pre-encrypt [M] [^3] | QEMU, Kata/CoCo | upstream [^39] |
| td-shim | TDX | 140 KB, 270 KB with secure boot [^37] | not published | CH (when re-enabled), libkrun-tdx main, Gramine-TDX | last tag v0.8.0 (Oct 2023), main active; no SNP (issue #135) [^37] |
| Oak stage0 | SNP and TDX | <= 2 MiB Rust, no EFI/BIOS services | not published | CH v52 KVM-SNP via IGVM, Alioth [^38] | active; boots bzImage or ELF; DICE measurements |
| SEVeriFast verifier | SNP | 13 KB | 8 ms pre-encrypt, 20-33 ms verify [M] [^3] | Firecracker fork | research prototype |
| qboot | SNP (libkrun-sev), TDX (libkrun-tdx) | small | not published | libkrun [^29], [^40] | TDX limited to 1 vCPU / 3 GiB |

td-shim measures a built-in payload into MRTD, a VMM-loaded payload and
cmdline into RTMR[^1] and the HOB into RTMR[^0], with a CCEL log [^37].
SEVeriFast criticises it for measuring its payload rather than hashing it
[^3].

Oak stage0 has two traps. First, `validate_measured_boot` is behind the
`sev_kernel_hashes` cargo feature. A default build logs "skipping" and
returns true, so the hash page is measured but never enforced. `urunc` must
build stage0 with that feature [^38]. Second, stage0 is not 13 KB: QEMU
`-bios` measures the whole ROM (up to 2 MiB), and CH measures all populated
IGVM pages. At ~4 MB/s PSP throughput that is 75-500 ms per launch [D] [^3],
serialized across concurrent launches.

### 3.3 Guests

**Linux CVM guest requirements.** The kernel needs
`CONFIG_INTEL_TDX_GUEST` or `CONFIG_AMD_MEM_ENCRYPT`, plus the guest drivers.
The unified configfs-tsm report interface exists since 6.7. Unaccepted
memory exists since 6.5, with `accept_memory=lazy` as the default [^48].
Linux forces `virtio_require_restricted_mem_acc` and sizes SWIOTLB at 6% of
RAM, clamped to [64 MB, 1 GB] [^48]. TCB sizes: Linux 6.8 is 13 MB / 2.3
MLoC; the TDX module is 45 KLoC [M] [^5].

**Enlightenment checklist for any non-Linux guest.**

- SNP: C-bit page tables, `PVALIDATE`, GHCB and a `#VC` handler
  (CPUID/MSR/IO/MMIO), page-state changes for shared buffers, SNP guest
  request for reports.
- TDX: `#VE` handler, TDCALL/TDVMCALL for I/O, `PAGE.ACCEPT`, MapGPA,
  `TDG.MR.REPORT` plus a host quote.
- Both: virtio rings and buffers in shared memory.

Porting size: about 2 KLoC of TDX-specific code in Gramine-TDX [^7]; 3,401 LoC
to make Unikraft SNP-capable (IncognitOS) [^9].

| Guest | SNP | TDX | Notes |
|---|---|---|---|
| Hermit | `hermit-sev` kernel fork (`amd-sev`, PVH boot, GHCB, SEVeriFast support) plus Firecracker (`v1.17.0-sev`) and rust-hypervisor-firmware (`sev-snp-directboot`) forks [^43] | none | Nothing upstream; daily commits Sept-Oct 2026. Preprint [^8]: compute overhead ~0, network 1.3-12x, TCB 9.65-34.7 MB (OVMF 4.19 MB) vs 245-270 MB for an Ubuntu CVM [M]. `urunc` runs Hermit on QEMU today. Best-aligned research stack. |
| Unikraft | PR #1094 (SEV/SEV-ES/SNP via `libuksev`, OVMF/EFI boot), open since Sept 2023, touched Apr 2026 [^41]; IncognitOS fork on Unikraft 0.15 with QEMU+OVMF [^9] | issue #1528, no code [^42] | No commercial CC offering found. |
| Gramine-TDX | none | td-shim payload, QEMU q35, PCI virtio-vsock/fs/console, 1.2 MB binary, ~57 KLoC TCB [^7] | Boot 2.8 s at 1 GB due to eager memory init [M] [^7]. Experimental, 4 commits since 2024. |
| Others [^52] | Nanos: plain SEV only. TamaGo: as a UEFI app on OVMF. Hyperlight: parked [^31]. MirageOS/Solo5, OSv, Mewz, Toro: none | none | Enarx dormant since mid-2024; Mystikos archived 2026-01; Veracruz archived 2026-02; Occlum SGX only |
| Oak restricted kernel | yes, on stage0 | stage0_tdx | single enclave application, QEMU microvm [^38] |

**LiteBox is not a standalone minimal guest.** LiteBox (Microsoft, Rust
libOS, v0.1 Feb 2026) runs on SNP at VMPL2 under a VMPL0 Linux kernel
driver, which receives its syscalls via a VMMCALL "VTL call" [^44]. It is a
sandbox inside a CVM, so VMPL0 Linux stays in the TCB. No TDX, no published
numbers. Wallet [^6] (VMPL-1 trustlets under
COCONUT-SVSM) and NestedSGX [^17] belong to the same family.

**OpenHCL and COCONUT-SVSM.** OpenHCL is a paravisor (Linux kernel plus Rust
in VTL2) [^46]. COCONUT-SVSM runs at VMPL0 and offers a vTPM; it still needs
a forked host kernel and QEMU branch, while its guest side is upstream [^45].
Both add TCB and a boot stage. They help only when a vTPM or an
unenlightened guest is required, and are not on the fast path.

### 3.4 Kata/CoCo costs

| Item | Value | Tag | Source |
|---|---|---|---|
| Pod start, kata-qemu-tdx (warm) | 8.85 s | [M] | [^10] |
| Memory per CoCo pod | ~2,075 MB | [M] | [^10] |
| TDX attestation, end to end | 24 ms | [M] | [^10] |
| TDX pod boot with OVMF 202608, 256 Mi to 128 Gi | 6-35 s | [M] | [^34] |
| OVMF 202511 eager accept, 64 GB TD | > 90 s | [M] | [^34] |
| `shared_fs=none` copy into guest RAM via agent RPC | ~85 MiB/s; rule "guest RAM >= 2x(image+volume)" | [M] | [^35] |
| In-guest image pull and decrypt | +3-5 s, serialized | [M] | [^1] |

AKS confidential containers are still in preview. They run Kata-CC on Cloud
Hypervisor with MSHV and reserve a 2 GB utility VM plus pod limits [^50].
Confidential GKE uses node-level CVMs, not per-pod CVMs [^51]. Edgeless
Contrast is a Kata fork that uses IGVM or firmware plus kernel and
dm-verity; it publishes no boot numbers [^49].

## 4. Hardware cost model

These per-unit costs drive every estimate in section 6.

**SNP launch (PSP).** `SNP_LAUNCH_UPDATE` measures and encrypts at about
4 MB/s on Milan [D] [^3]: a 23 MB vmlinux took 5.65 s, a 12 MB initrd 2.85 s,
a 1 MB OVMF 257 ms [M] [^3]. The PSP is single-core and serializes launches
across VMs: 50 concurrent launches average 1.8 s [M] [^3]; 200 SEV microVMs
take 28.5 s [M] [^4]. The slope is 33 ms/VM on Firecracker and 556 ms/VM on
QEMU [M] [^3]. Rule: never pre-encrypt the kernel and initrd. Measure a small
root of trust and hash-check the rest in the guest.

**TDX launch.** `TDH.MEM.PAGE.ADD` works on 4 KB pages only.
`KVM_TDX_MEASURE_MEMORY_REGION` issues 16 `TDH.MR.EXTEND` calls per page,
roughly 5-10 ms/MB [E]. The shim is always measured: td-shim 140-270 KB [^37],
TDVF ~4 MB [^5]. Host-side guest_memfd setup costs ~80 ms/GB on SNP and
~28 ms/GB on TDX [M] [^5].

**Memory acceptance.** Lazy accept (`TDG.MEM.PAGE.ACCEPT` after `PAGE.AUG`)
costs 2.5-8 us per 4 KB page, i.e. 0.64-2 ms/MB [D] [^19], [^20]. SNP
`PVALIDATE` costs 24 us per 4 KB page in Wallet's setup [M] [^6]; with THP,
validating 256 MB drops from >60 ms to <1 ms [M] [^3].

Lazy acceptance does not help a non-EFI direct boot. Linux discovers
unaccepted memory only through the EFI config table
`LINUX_EFI_UNACCEPTED_MEM_TABLE_GUID` (`arch/x86/boot/compressed/mem.c`)
[^48]; td-shim's `boot_params` bitmap is the abandoned pre-6.5 proposal [^37].
On every non-EFI path (stage0, td-shim Linux boot), and for TDVF guests
below its ~512 MB pre-accept, acceptance is eager, so `accept_memory=lazy`
buys nothing in stage 1. Stage0 validates all e820 RAM, trying 2 MiB and
falling back to 4 KiB [^38]; guest_memfd's order-0 folios make 2 MiB fail
(CH documents no SNP hugepages [^24]).
Eager cost per 4 KB page is 1.5-5 us on SNP (including the gmem fault and
`RMPUPDATE`) and 3-10 us on TDX (AUG plus ACCEPT) [E]. For 128 MB that is
50-160 ms (SNP) or 80-330 ms (TDX) [D from the per-page estimate]; double
for 256 MB.

**fw_cfg through stage0.** Oak stage0 reads fw_cfg through one 4 KiB shared
DMA bounce page [^38]. Each 4 KB of kernel or initrd costs port-I/O exits
(`#VC`/`#VE` to VMGEXIT/TDVMCALL to userspace, 8-20 us each [E]) plus a copy
and SHA-256, twice with `sev_kernel_hashes`. That is about 5-15 ms/MB [E],
or 50-250 ms for 10-20 MB [E].

**Exits and the console.** A TD CPUID/MSR/IO exit costs ~12-15 us versus
~2 us in a normal VM; an SNP exit costs ~4 us [M] [^5]. VMEXIT cost rises
240% (SNP) and 472% (TDX) [M] [^5]. `console=ttyS0` costs one exit per
character, 8-40 us [E]; a 20-50 KB boot log costs 0.15-2 s [D]. TEE mode
needs `quiet loglevel=0` or virtio-console (`console=hvc0`), no
earlyprintk and `CONFIG_MAGIC_SYSRQ=n`.

**Guest Linux.** Linux boots about 2.3x slower under SNP [M] [^3]. First
memory touch costs +59% (SNP) and +92% (TDX) [M] [^5].

**SWIOTLB.** The default is 6% of RAM clamped to [64 MB, 1 GB] [^48]. In a
128 MB guest that is 50% of RAM and 50-160 ms to convert to shared [E].
Private-to-shared conversion costs 0.8-2.5 ms/MB on 1 vCPU [E], far more
with many vCPUs because of TLB-shootdown IPIs. `swiotlb=` takes a slab count
of 2 KB slabs, so `swiotlb=4M` parses as 4 slabs. Use `swiotlb=2048` (4 MB)
to `8192` (16 MB). SWIOTLB also costs iperf throughput: -21% (SNP) and -60%
(TDX) [M] [^5].

**Attestation primitives.**

| Step | Cost | Tag | Source |
|---|---|---|---|
| SNP attestation report | 5.88 ms [^6]; 6.19 ms [^5] | [M] | [^5], [^6] |
| TDX TDREPORT | 0.022 ms | [M] | [^5] |
| TDX quote (vsock QGS path) | 2.75 ms [^5]; ~7 ms [^10] | [M] | [^5], [^10] |
| Linux `tdx-guest` GetQuote poll interval | 1 s | [M] | [^48] |
| Report verification | SNP 1.04 + 2.68 ms; TDX 0.67 + 0.24 ms | [M] | [^5] |
| KBS round trip on a LAN | 5-30 ms | [E] | |
| Cold AMD KDS VCEK fetch | 427 ms | [M] | [^13] |
| Cold KDS/PCS collateral fetch, general | 100-500 ms | [E] | |
| Differential attestation (Wallet) | 0.25 ms | [M] | [^6] |

Collateral must be cached per node and kept off the per-VM path.

**Memory overheads.** All guest RAM is committed at boot, with no KSM or
overcommit. Losing KSM costs +88-128% memory across 4 CVMs [M] [^12].
Caching 500 SEV microVMs needs 42.5 GB versus 12.5 GB with page sharing,
about 85 MB versus 25 MB each [M] [^4].
SEVeriFast adds 16 KB of host memory per SEV microVM [M] [^3].

guest_memfd folios are charged to the faulting vCPU's memcg, so guest RAM
equal to the container limit (`monitorMemoryBytes`) OOM-kills the VMM. Guest
RAM must be the limit minus VMM RSS (10-30 MB [E]), shared pages and
headroom.

**Concurrency caps.** SNP ASIDs (~509 on Milan, ~1006 on Genoa) and TDX
private HKIDs (BIOS-partitioned, often tens to low hundreds) bound CVMs per
host regardless of RAM [E, platform documentation not recorded].

**Teardown.** `RMPUPDATE` back per page, DF_FLUSH/WBINVD, and TDX cache
write-back on HKID free add 30-100 ms per 256 MB and host jitter [E].

## 5. Literature

**Segarra et al., SESAME'24 [^1].** "Serverless Confidential Containers:
Challenges and Opportunities". CC-Knative on CoCo 0.7, Kata, QEMU and OVMF,
plain SEV (not SNP), 2 GB guest. Cold start 17.5 s versus 6 s for runc and
7 s for Kata [M]. Scaling 0 to 16 takes 190 s versus 16 s [M]. The +4 s of
CVM over VM splits into ~1 s of QEMU pinning 2 GB (linear in RAM, ~30 s at
128 GB), ~3 s of OVMF DXE (20x slower under SEV) and 0.7 s of Linux [M].
In-guest image pull and decryption add 3-5 s and are serialized [M]. Warm
starts do not help because SEV CVMs are bound to a guest owner. The paper
lists four challenges:

- **C1**: provision a CVM faster;
- **C2**: provision an encrypted image faster;
- **C3**: reuse a CVM securely;
- **C4**: share encrypted images.

SNP/TDX guest-initiated attestation makes pre-warming feasible, limited by
pinning and `PVALIDATE` hot-plug cost. `urunc`'s own
SESAME'24 paper measured a Knative cold start of 1.21 s for `urunc` versus
1.20 s for runc, and 2.2-3.9 s for Kata and gVisor [M] [^2]. SESAME'25,
SESAME'26 and WoSC have no CC papers.

**CoFunc, USENIX Security'25 [^4].** OVMF is 4,905 ms of the 6,308 ms
Kata-CVM boot on SEV. A SEVeriFast microVM cuts it to 334 ms (SEV init 78,
Firecracker 53, verifier 74, Linux 129). Memory is 4.7 MB per container
versus 85 MB per Kata microVM. All [M]. The design uses one pre-launched CVM per tenant, zygote fork
and split attestation.

**Wallet, NSDI'26 [^6].** Cold start 2.38 s, zygote fork 10.3 ms, SHA-512 of
a 61 MB kernel 1.15 s, TCB 1.1 MLoC versus 4.8 MLoC. All [M].

**SEVeriFast, ASPLOS'24 [^3].** Replaces OVMF with a 13 KB verifier that is
pre-encrypted (8 ms) and then hash-checks the kernel (20-33 ms). Time to init
on Firecracker SNP is ~170-320 ms versus 28-147 ms without SEV, depending on
the kernel. All [M]. The PSP slope, the 2.3x Linux slowdown and the THP
result are in section 4.

**Misono et al., SIGMETRICS'25 [^5].** On QEMU 8.2, 8 vCPU, 8 GB, the TD boot
splits into QEMU 0.32 s, TDVF 1.05 s, Linux 4.14 s and init 5.15 s; the SNP
boot into QEMU 1.25 s, OVMF 3.81 s, Linux 1.41 s and init 2.30 s. QEMU-side
time scales with RAM: 7.34 s for a 256 GB TD. All [M]. The paper calls for
"new boot schemes" for serverless.

**Niroula et al. [^12].** In-CVM container creation is 65% of function time;
cgroups, seccomp and netns take 30-50 ms [M]. This supports an agentless,
single-process guest.

**Gramine-TDX [^7] and Hermit-SEV [^8]** are covered in section 3.3. Hermit-SEV
is faster than a Linux CVM at small RAM and slower above 8 GiB, because it
maps all memory at boot; exact milliseconds appear only in figures.

**Others.** Reducio [^11] cites Kata CVM 6.8 s, Kata micro-CVM 334 ms,
SEVeriFast 325 ms and Gramine-TDX 2.98 s (secondary numbers). Techniques in
the literature: tiny verifier instead of UEFI [^3], td-shim [^7], lazy accept
[^19], 2 MB `PVALIDATE` [^3], hash-not-encrypt [^3], shared CVM plus zygote
[^4], [^6], [^11], differential attestation [^6], pre-warmed pools [^1], removing
the in-guest runtime [^12], zero-copy I/O (Bifrost) [^18], VCEK caching [^13]
and a minimal PSP footprint [^3].

Reported CVM boot numbers:

| System | TEE | Scope | Reported | Source |
|---|---|---|---|---|
| CC-Knative on CoCo 0.7 | SEV | function cold start | 17.5 s | [^1] |
| CoCo kata-qemu-tdx | TDX | pod start, warm | 8.85 s | [^10] |
| Kata-CVM, QEMU+OVMF | SEV | VM boot | 6.3 s | [^4] |
| Kata TDX | TDX | container init | 1.8 s | [^4] |
| QEMU 8.2 + TDVF, 8 GB | TDX | boot to init | 10.66 s (2.80 s normal) | [^5] |
| QEMU 8.2 + OVMF, 8 GB | SNP | boot to init | 8.77 s (4.27 s normal) | [^5] |
| Wallet CVM | SNP | boot | 8.3 s (3.7 s normal) | [^6] |
| Gramine-TDX, 1 GB | TDX | hello-world | 2.8 s | [^7] |
| QEMU+OVMF | SNP | firmware only | 3.2 s + 288 ms | [^3] |
| Kata micro-CVM + SEVeriFast | SEV | VM boot | 334 ms | [^4] |
| SEVeriFast, Firecracker | SNP | to init | 170-320 ms | [^3] |

All values [M].

Non-confidential microVM boot numbers:

| System | Reported | Source |
|---|---|---|
| Unikraft helloworld, Firecracker / Solo5 | 3.1 ms | [^15] |
| Unikraft helloworld, QEMU microvm | 9.1 ms | [^15] |
| Unikraft helloworld, QEMU pc | 38 ms | [^15] |
| MirageOS | 1-2 ms | [^15] |
| Lupine | 23 ms | [^16] |
| Stock Firecracker, tuned kernels | 28-47 ms | [^3] |
| Firecracker + Linux | ~100-150 ms | [^14] |
| Cloud Hypervisor | ~100 ms | [^14] |
| QEMU + qboot | ~200 ms | [^14] |
| Kata | ~600 ms | [^55] |
| `urunc` Knative cold start | 1.21 s (runc 1.20 s) | [^2] |

All values [M].

## 6. Feasibility verdict and estimates

**Feasible, in three stages.** The gain is real but smaller than a naive
reading suggests. Its three sources are listed in section 1. CoCo cannot
adopt them without a redesign. No unikernel that `urunc` supports has
upstream TDX/SNP support, so stage 1 uses the single-process Linux guest.

**Scope mismatch.** A CoCo pod start includes kubelet, CNI, the pause
container, VM boot, the agent and an in-guest image pull. Our `urunc`
estimates cover only VMM exec to application entry. Comparisons against a
pod start must say so.

Estimated cold boot, VMM exec to application entry, 1 vCPU, 128 MB guest,
10-20 MB kernel plus initrd, quiet console, attestation excluded. All [E]
unless marked [M]. The fixed CVM costs (PSP launch, eager 4 KB validation or
acceptance, VMM and TD/ASID setup) dominate and do not depend on the guest.

| Configuration | Estimate | Dominant terms |
|---|---|---|
| CoCo kata-qemu-tdx pod start (wider scope) | 6-9 s, ~2 GB RAM [M] [^10], [^34] | OVMF/TDVF, full Linux, agent, image pull |
| Kata-CVM VM boot, SEV (QEMU+OVMF) / TDX | 6.3 s / 1.8 s [M] [^4] | OVMF 4.9 s; TDVF and accept |
| Kata micro-CVM with SEVeriFast firmware (research) | 334 ms [M] [^4] | SEV init 78, FC 53, verifier 74, Linux 129 |
| **`urunc` + QEMU 10.1 SNP, microvm + stage0 (`sev_kernel_hashes`) + Linux** | **0.35-0.8 s** [E]; >1 s if the full 2 MiB ROM is measured or at 256 MB | PSP launch 75-500 ms, eager `PVALIDATE` 50-160 ms, fw_cfg 50-250 ms, Linux 100-200 ms (2.3x non-CVM), VMM 20-50 ms |
| `urunc` + QEMU SNP, q35 + OVMF AmdSev + `kernel-hashes=on` (reference path) | 1.5-3.5 s [E] | OVMF SNP runtime 1.1-3.2 s [M] [^3], [^5] |
| `urunc` + Cloud Hypervisor v52 SNP + stage0 IGVM + Linux | 0.35-0.8 s [E] | as QEMU/stage0; CH start 10-50 ms shorter |
| `urunc` + QEMU 10.1 TDX + TDVF + `-kernel` + Linux, 256 MB (only upstream TDX path) | 0.7-1.8 s [E] | TDVF DXE 0.3-0.5 s, eager accept 0.16-0.66 s, Linux 0.2-0.5 s |
| `urunc` + libkrun-tdx (td-shim, unreleased) or future CH TDX + td-shim + Linux | 0.4-1.1 s (128 MB) / 0.5-1.4 s (256 MB) [E] | shim `PAGE.ADD`/`MR.EXTEND`, RTMR hashing of 15 MB 75-150 ms, eager accept, Linux |
| `urunc` + SNP + unikernel (Hermit-SEV / Unikraft-SNP; needs a research VMM fork or OVMF today) | 0.2-0.6 s [E] | fixed CVM costs; guest itself 1-30 ms |
| `urunc` + TDX + td-shim + unikernel (nothing like it exists) | 0.15-0.4 s [E] | shim measurement 70-160 ms, TD create 50-100 ms, accept |

Add 10-40 ms for attestation with cached collateral [E]. Earlier drafts
assumed lazy acceptance and a 13 KB root of trust; this table corrects both.

Headline claims that survive review:

- **3-10x faster than a Kata-CVM VM boot** (1.8-6.3 s [M] [^4] to 0.35-0.8 s
  [E] on SNP). **10-20x faster than a stock CoCo pod start** (wider scope,
  see above). Roughly **equal to a SEVeriFast-tuned Kata micro-CVM** [^4],
  which nobody ships.
- **A unikernel guest buys only ~1.3-2x more latency** [E] over
  single-process Linux, because fixed CVM costs dominate. Its value is
  memory (2-30 MB [E] versus 85 MB per Kata microVM [M] [^4] versus ~2 GB per
  CoCo pod [M] [^10]) and TCB (1-10 MB [E] versus a 13 MB kernel plus 4 MB
  firmware [M] [^5] plus the agent stack).
- **Memory density 4-12x** [E] for small images, not 15x. The initrd rootfs
  lives in guest RAM (~2x during unpack), all RAM is committed at boot, VMM
  RSS and shared pages add 10-30 MB, there is no KSM or overcommit, and
  ASID/HKID counts cap CVMs per host.
- **SNP burst launches are PSP-bound.** The slope is 33 ms/VM with a 13 KB
  root of trust [M] [^3] and 75-500 ms/VM with stage0 [D]. With 20
  concurrent starts the last starts 1.5-10 s behind the first [D]. TDX has no
  equivalent serialization point [^4].

**Must measure first on hardware.** The estimates are anchored on
SEVeriFast [^3], CoFunc [^4], Misono [^5] and Gramine-TDX [^7]. These inputs are
the most uncertain:

1. populated stage0 IGVM page count times `LAUNCH_UPDATE` rate on Genoa and
   Turin;
2. per-4 KB `PVALIDATE`/accept cost with guest_memfd, and with 2 MB
   mappings;
3. fw_cfg throughput through stage0;
4. console exit cost;
5. `TDH.MEM.PAGE.ADD`/`MR.EXTEND` cost per page;
6. private-to-shared conversion on 1 vCPU.

## 7. Proposed design

### 7.1 Architecture: a "confidential unikontainer"

**1. Measured immutable image plus a per-container config blob.** The OCI
image carries the kernel, the initrd and a fixed cmdline template. Reference
values (and, for SNP, an ID block) are produced at image build time and
published with provenance (cosign/in-toto, RVPS), never on the host. The
launch digest covers the firmware (stage0 or TDVF) and the
kernel/initrd/cmdline hash page (QEMU `kernel-hashes=on` [^21], CH PR #8123
[^25]; TDVF measures `-kernel` into RTMR[^1] [^22]).

Per-container data that `urunc` controls today through the cmdline and
initrd mutation (IP/gateway/mask, env, cmd, uid/gid, workdir, mounts) moves
into a **config blob on a raw virtio-blk device**. This is the pattern
`urunc` already uses for the FreeBSD `urunit` config device
(`unikernels/freebsd.go`). Its SHA-256 goes into SNP `HOST_DATA` (32 B) or
its SHA-384 into TDX `MRCONFIGID` (48 B). `urunit` fetches its own report
through configfs-tsm (~6 ms on SNP [M] [^6], 0.02 ms TDREPORT on TDX [M], section 4)
and refuses to run unless `sha(blob)` equals `HOST_DATA`/`MRCONFIGID`.

This keeps one reference measurement per image, mirrors CoCo "initdata"
[^36], and takes IP and env off the measured cmdline. The initrd is no longer
mutated per container.

Other agentless binding patterns exist: the whole image in the launch
digest (td-shim payload, SNP `LAUNCH_UPDATE`, IGVM) and measured direct boot
alone. The Kata agent-plus-policy pattern is what `urunc` avoids by design.

**2. A verifier always gates the workload.** There is no agent and no
attestation daemon, but a digest nobody verifies gives no assurance. Every
TEE container needs a relying party that gates something. It either (a) releases a secret from a KBS/Trustee [^54] (a
dm-crypt key, an API token, env secrets, which are plaintext on the host in
OCI and must not be passed as OCI env), or (b) binds its identity (RA-TLS or
an attested certificate) before it serves traffic. The verifier policy
checks:

- the launch digest and `HOST_DATA`/`MRCONFIGID`;
- TCB SVNs and TCB status;
- SNP policy `debug=0` and `MIGRATE_MA=0`;
- TDX attributes `DEBUG=0` and `SEPT_VE_DISABLE=1`, XFAM, `MRSIGNERSEAM`;
- a nonce in `REPORT_DATA`.

A one-shot Trustee client in the initrd (not inside PID 1) does this.

**3. Monitors, by stage.**

| Stage | Monitor | Firmware | Notes |
|---|---|---|---|
| 1 (SNP) | QEMU >= 10.1, `sev-snp-guest,kernel-hashes=on,host-data=` | q35 + OVMF AmdSev | reference and bring-up path |
| 1 (SNP) | QEMU >= 10.1, `microvm,acpi=on` | `-bios` stage0 built with `sev_kernel_hashes` | fast path |
| 1b (SNP) | Cloud Hypervisor >= v52 KVM-SNP | stage0 IGVM | host-data needs a per-image IGVM rebuild or an upstream flag |
| 2 (TDX) | QEMU, `tdx-guest,mrconfigid=`, q35, `kernel-irqchip=split`, x2APIC | TDVF + `-kernel` | only upstream TDX path |
| 2 (TDX, fast) | libkrun-tdx behind a `krun`-style launcher binary, or CH TDX after re-enablement | td-shim | depends on unreleased work [^27], [^29] |
| none | Firecracker, Hyperlight, Solo5 | | no upstream support |

**4. Guests.** Stage 1 is single-process Linux with `urunit`, kernel >= 6.11,
with `CONFIG_AMD_MEM_ENCRYPT`, `CONFIG_SEV_GUEST`,
`CONFIG_INTEL_TDX_GUEST`, `CONFIG_TDX_GUEST_DRIVER`, `CONFIG_TSM_REPORTS`,
`CONFIG_X86_X2APIC`, `CONFIG_MAGIC_SYSRQ=n` and virtio-mmio for microvm. The
cmdline template is `quiet loglevel=0 swiotlb=2048 panic=-1 ...`, and QEMU
runs with `-no-reboot` so a panic exits the VMM. CVM reset is unsupported in
CH and in QEMU before 11.0. Stage 3 is a unikernel: Hermit via the hermit-sev
work [^43], which must be rebased on upstream KVM SNP, guest_memfd and host
6.11 (today it needs a Firecracker or rust-hypervisor-firmware fork). Then
Unikraft, if PR #1094 lands [^41].

**5. Rootfs in confidential mode.** Allowed: `initrd` (measured) or none;
`block` only as dm-verity (root hash in the config blob, checked by
`urunit`) or dm-crypt with integrity (`--integrity aead`, key from the KBS).
Rejected: read-write plaintext devmapper snapshots, plaintext block images,
9p, virtiofs and vAccel.

**6. Mapping to SESAME'24 C1-C4 [^1].**

| Challenge | This design |
|---|---|
| C1, faster CVM | minimal firmware, small RAM, no OVMF on the fast path |
| C2, faster encrypted image | the image is the measured kernel plus initrd; no in-guest pull; encrypted payloads are decrypted after attestation |
| C3, reuse a CVM | not possible per container under OCI. The only route is a CoFunc/Wallet-style shared per-tenant CVM with fork [^4], [^6], a different runtime model. Future work. |
| C4, share encrypted images | not applicable |

CoFunc's host-side "shadow container" for cgroups [^4] is what `urunc`
already is.

Pre-warming is dropped. OCI has no start-time inputs (the config is complete
at `create`), and `HOST_DATA` is fixed at `LAUNCH_FINISH`. A VM pre-warmed
before `create` could only receive its config after attestation.

### 7.2 Known security gaps

- ACPI tables and e820 are host-supplied. They appear only in stage0's DICE
  evidence [^38]; TDVF measures them into RTMR[^0].
- Time is host-controlled without Secure TSC.
- TDX quotes depend on the host QGS. This is a denial-of-service risk only.
- The console is a plaintext, bidirectional channel. Stage 2 adds a
  policy-controlled `--serial off` and virtio-console.
- The cmdline is host-visible and measured: IP, gateway and mask, plus env
  if `urunit` is not used.
- In stage 1 the mutated initrd sits in plaintext in the snapshot. Stage 1
  gives integrity, not confidentiality, of env and config. Secrets must be
  released after attestation.

## 8. Implementation roadmap

Stages: **1** QEMU SNP; **1b** Cloud Hypervisor SNP; **2** TDX via TDVF, then
td-shim via libkrun or CH; **3** unikernel.

Facts that shape the cut. Both execution paths (`Exec` in
`unikontainers.go:624` and `runMonitor` in `monitor.go:69`) consume
`ExecArgs` built only in `buildMonitorSpec`. `mountsForMonitor`
(`rootfs.go:488`) and `getMonitorDevices` (`rootfs.go:419`) are persisted in
`monitor.json` and applied by both paths. There is no `urunc` process left
after `execve`, so a guest "ready" timestamp cannot be a TS19 in `urunc`.

All PRs below are buildable and unit-testable without TEE hardware.

- **PR0, deterministic initrd records** (`pkg/unikontainers/initrd/initrd.go`).
  `AddFileToInitrd` stamps `time.Now()` into the cpio header and
  `copyFileToInitrdWithInode` copies host mtimes, so every launch yields a
  different hash. Set mtime to 0 for `urunc`-written records and sort bind
  mounts by destination in `CopyFileMountsToInitrd`. Test: identical inputs
  give byte-identical archives. Independent; mergeable now.
- **PR1, types, config and annotation; no behaviour change**
  (`types/types.go`, `urunc_config.go`, `annotations.go`).
  `ExecArgs.TEE TEEParams{Type, FirmwarePath, ConfigBlobPath, ConfigDigest}`,
  `UnikernelParams.TEE`, and `MonitorConfig` fields `FirmwarePath`
  (`firmware_path`), `TEE` (`tee`) and `Machine` (`machine`). Annotation
  `com.urunc.unikernel.tee` is read only from spec annotations (`json:"-"`,
  the vAccel pattern), so an image cannot demand a TEE.
  `supportedTEEMonitorPairs`: {qemu, sev-snp}, {qemu, tdx},
  {cloud-hypervisor, sev-snp}; the guest must be `linux`. Tests include
  "does not fall back to runc".
- **PR2, create-time plumbing and policy** (`unikontainers.go`, `rootfs.go`,
  `unikernels/linux.go`, `unikernels/freebsd.go`). `InitialSetup` resolves
  the TEE, checks the firmware file and fails fast on host capability
  (`/dev/sev` and `kvm_amd.sev_snp`, or `kvm_intel.tdx`).
  `validateTEERootfs` runs after `ChooseRootfs`. TEE memory is the cgroup
  limit minus headroom. `mountsForMonitor` read-only binds `FirmwarePath`;
  `getMonitorDevices` adds `/dev/sev`. In TEE mode `linux.go` emits the fixed
  cmdline (`quiet loglevel=0 swiotlb=2048 panic=-1 console=... root=/dev/ram0
  rdinit=/urunit URUNIT_CONFIG=/dev/vdX`), with no `ip=` and no env, and
  builds the sector-padded config blob (reusing the FreeBSD code) and its
  digest. A companion `urunit` change verifies the digest.
- **PR3, QEMU SNP/TDX argv, the primary target** (`hypervisors/qemu.go`).
  Machine selector (`pc`, `q35`, `microvm`, arm64 `virt`); `-bios` from
  `FirmwarePath`; SNP objects (`sev-snp-guest,id=sev0,cbitpos=51,
  reduced-phys-bits=1,kernel-hashes=on,host-data=<digest>`,
  `memory-backend-memfd`, `confidential-guest-support=sev0`); TDX objects
  (`tdx-guest,id=tdx0,mrconfigid=<digest>`, q35, `kernel-irqchip=split`);
  virtio devices with `iommu_platform=on,disable-legacy=on`; mutually
  exclusive with the virtiofs `memory-backend-file`/`-numa` branch. This also
  unlocks the non-confidential firmware experiment.
- **PR4, Cloud Hypervisor SNP argv, stage 1b**
  (`hypervisors/cloud_hypervisor.go`). `teeArgs()` emits `--platform
  sev_snp=on --igvm <fw>`, always `--kernel` and `--cmdline`, a TEE-aware
  `--memory` and a shared-fs rejection. Argv tests with `fakeUnikernel`.
- **PR5, launch manifest, design doc, tooling.** Write
  `/.launch_manifest.json` into the monitor rootfs before `setupUser` (TEE
  type, firmware/kernel/initrd SHA-256, exact cmdline, appended records) so
  a relying party can recompute the digest. `script/performance/__modules__.py`
  merges a guest console marker from `nerdctl logs -t` with
  `/tmp/urunc.zlog`. Fix the stale `timestamps.md`.
- **PR6, e2e and deploy** (`tests/e2e`, `deployment/`, CI). TEE e2e cases
  `Skippable` behind `URUNC_E2E_TEE`; non-TEE firmware cases run in CI;
  `urunc-deploy` keys; CH >= v52 and firmware artifacts (kata-deploy 3.15.0
  already provides `AMDSEV.fd` and TDVF `OVMF.fd` [^33]; the Oak stage0 IGVM
  must be built).

Risks beyond the gaps in section 7.2:

- `panic=-1` reboots are impossible for CVMs in CH v52. Verify the VMM exits
  rather than hangs.
- `o+rw` device widening applies to `/dev/sev` in the private `/dev` tmpfs.
- `start` returns success before `execve`, so PSP or ASID failures show only
  as a dead VMM. Fail-fast checks at `create` mitigate this.
- QEMU virtio devices need `VIRTIO_F_ACCESS_PLATFORM`. TDX needs x2APIC and
  ACPI MP-wakeup for more than 1 vCPU. The virtiofs
  `memory-backend-file,mem-path=/tmp` branch conflicts with the memfd
  backend.
- Guest RAM equal to the memory limit OOM-kills the VMM (section 4).

## 9. Evaluation plan

The evaluation runs without TEE hardware. It isolates the firmware and
boot-path overhead on plain KVM, then combines it with the per-unit costs of
section 4 into a bottom-up confidential estimate. It needs the PR1/PR3 subset
(`machine`, `firmware_path`, the QEMU machine selector and `-bios`), the
firmware bind in `rootfs.go`, and the guest marker parsing in
`script/performance/__modules__.py`.

**The bundle.** A tarball, `urunc-cc-eval.tar.gz`, produced by a script
under `script/cc-eval/`:

| File | Role |
|---|---|
| `README.md` | prerequisites (Ubuntu 22.04/24.04 x86_64, KVM, containerd + nerdctl, docker or podman, 8+ cores), what each variant measures |
| `setup.sh` | installs QEMU (>= 9.1 for SNP, >= 10.1 for TDX), pinned CH and Firecracker, OVMF, Oak stage0 (with `sev_kernel_hashes`), `urunc` and config variants |
| `build-guest.sh` | builds a pinned LTS kernel with CC guest options and an initrd (busybox, `urunit`, an app printing `URUNC_GUEST_READY`), packages the images |
| `run.sh [-n ITER] [--variants ...]` | detects SNP/TDX, runs each variant N times (default 20), collects `urunc` timestamps and `nerdctl logs -t`, host facts and VMM RSS |
| `analyze.py` | median/p10/p90 per phase, firmware cost (variant minus `qemu-microvm-kernel`), bottom-up CC estimate with sourced constants, predicted versus measured; writes `summary.md` |
| `model.py`, `predictions.md` | the constants of section 4 and the table below |

Cloud Hypervisor with stage0 via IGVM but without SNP is blocked by CH issue
#8454 [^28], and `--firmware` cannot take `--kernel` [^26]. It is optional.

**How to run.** `setup.sh`, `build-guest.sh`, `run.sh`, `analyze.py`.
Validate on rented bare-metal SNP (Milan/Genoa) and TDX
(SPR/EMR/GNR) hosts. Cloud CVM instances cannot host nested SNP/TDX guests.
On an SNP host (Linux >= 6.11, QEMU >= 9.1) check that the `sev-snp-measure`
[^53] digest equals the report measurement and that `HOST_DATA` equals the
config blob digest. On a TDX host (Linux >= 6.16, QEMU >= 10.1) do the same
with TDVF and `MRCONFIGID`.

**Predicted results.** For a 1 vCPU, 128-256 MB guest with a 10-20 MB
kernel plus initrd on a modern x86 server. All [E], derived from the cited
measurements.

| Variant or phase | Isolates | Predicted | Reasoning |
|---|---|---|---|
| `fc-direct` | non-CC baseline | 40-150 ms | stock Firecracker 28-47 ms with Lupine/AWS kernels [^3]; ~100-150 ms with a generic kernel [^14] |
| `ch-direct` | today's CH path | 60-180 ms | CH process ~10-30 ms slower than Firecracker; same kernel boot |
| `qemu-microvm-kernel` | QEMU without firmware | 80-200 ms | microvm adds ~6 ms over Firecracker for a 2 MB image [^15]; QEMU start 30-80 ms |
| `qemu-pc-seabios` | today's QEMU path | 150-350 ms | SeaBIOS, PCI enumeration, i440fx; `pc` is ~29 ms slower than microvm for a tiny guest [^15], far more for Linux with PCI |
| `qemu-microvm-stage0` | stage0 and fw_cfg (SNP fast-path analogue) | microvm-kernel + 30-120 ms | fw_cfg DMA copy of 10-20 MB, stage0 init, e820/ACPI; no per-4 KB exits without CC |
| `qemu-q35-ovmf-kernel` | OVMF with `-kernel` (TDVF/AmdSev analogue) | microvm-kernel + 300-900 ms | OVMF SEC/PEI/DXE/BDS on plain KVM; normal-VM firmware phase 0.75-0.92 s [^5] |
| `urunc` TS00-TS18 | runtime overhead | 5-30 ms | `timestamps.md` shows ~1-12 ms create-side; initrd mutation dominates |
| Guest Linux to `urunit` marker | guest boot | 60-150 ms tuned; 200-400 ms distro-like | non-CC microVM boots [^3], [^14] |
| `qemu-microvm-stage0-snp` | TEE host only | 0.35-0.8 s; >1 s if the 2 MiB ROM is measured whole | section 6 |
| `qemu-q35-amdsev-snp` | TEE host only | 1.5-3.5 s | OVMF SNP runtime [^3], [^5] |
| `qemu-q35-tdvf-tdx`, 256 MB | TEE host only | 0.7-1.8 s | TDVF DXE, eager accept, Linux (section 6) |
| VMM RSS at marker | memory | QEMU 15-40 MB; CH/FC 5-15 MB, plus guest RAM | guest RAM fully resident only in TEE variants |

`analyze.py` prints predicted against measured and flags phases outside the
range, so the run either validates or corrects the model.

## 10. Open questions

1. Which exact Cloud Hypervisor v52.x flags, if any, will carry host-data
   and the ID block, or must `urunc` rebuild the IGVM per image?
2. Do `/etc/hosts`, `/etc/hostname` and `/etc/resolv.conf` stay in the
   measured initrd (the verifier reconstructs them), or are they derived in
   the guest from the config blob?
3. Firmware provenance in `urunc-deploy`: Oak stage0 built from source
   versus kata-deploy OVMF for QEMU.
4. Is a RuntimeClass-level `tee` default acceptable, or must the pod
   annotation be explicit?
5. Should `MonitorCapabilities` be narrowed in the libcontainer path?
6. When will TDX return to Cloud Hypervisor (Cyberus/Intel work [^27]) and
   td-shim support reach a libkrun release [^29]?
7. Does `panic=-1` exit or hang a CVM in CH v52?

## 11. References

[^1]: C. Segarra, T. Feldman-Fitzthum, D. Buono, P. Pietzuch. "Serverless
    Confidential Containers: Challenges and Opportunities." SESAME'24
    (EuroSys workshops). <https://doi.org/10.1145/3642977.3652097>
[^2]: C. Mainas, I. Plakas, G. Ntoutsos, A. Nanos. "Sandboxing Functions for
    Efficient and Secure Multi-tenant Serverless Deployments." SESAME'24.
    <https://doi.org/10.1145/3642977.3652096>
[^3]: B. Holmes, J. Waterman, D. Williams. "SEVeriFast: Minimizing the Root of
    Trust for Fast Startup of SEV microVMs." ASPLOS'24.
    <https://doi.org/10.1145/3620665.3640424>,
    <https://people.cs.vt.edu/djwillia/papers/asplos24-severifast.pdf>
[^4]: J. Shi, J. Gu, Y. Xia, H. Chen. "Serverless Functions Made Confidential
    and Efficient with Split Containers" (CoFunc). USENIX Security'25.
    <https://www.usenix.org/conference/usenixsecurity25/presentation/shi-jiacheng>,
    <https://ipads.sjtu.edu.cn/zh/publications/usenixsecurity25-shi-jiacheng.pdf>
[^5]: M. Misono, D. Stavrakakis, N. Santos, P. Bhatotia. "Confidential VMs
    Explained: An Empirical Analysis of AMD SEV-SNP and Intel TDX." POMACS
    8(3) / SIGMETRICS'25. <https://doi.org/10.1145/3700418>,
    <https://dse.in.tum.de/wp-content/uploads/2024/11/sigmetrics25summer-CVM-Explained.pdf>
[^6]: M. Sabanic, M. Misono, T. Bodea, et al. "Wallet: Confidential Serverless
    Computing." NSDI'26. <https://www.usenix.org/conference/nsdi26/presentation/sabanic>,
    <https://arxiv.org/abs/2504.21518>
[^7]: D. Kuvaiskii, D. Stavrakakis, K. Qin, C. Xing, P. Bhatotia, M. Vij.
    "Gramine-TDX: A Lightweight OS Kernel for Confidential VMs." CCS'24.
    <https://doi.org/10.1145/3658644.3690323>,
    <https://github.com/gramineproject/gramine-tdx>
[^8]: "Combining Confidential Computing and Unikernels for Lightweight
    Confidential Computing." Preprint, Nov 2025.
    <https://doi.org/10.5281/zenodo.17493275>
[^9]: IncognitOS (self-obfuscating unikernel on SEV-SNP, Unikraft 0.15 fork).
    IEEE S&P'25. <https://doi.org/10.1109/SP61157.2025.00222>,
    <https://github.com/sslab-skku/incognitos>
[^10]: dstack-capsule. arXiv 2606.03323. <https://arxiv.org/abs/2606.03323>
[^11]: Reducio. arXiv 2608.30171. <https://arxiv.org/abs/2608.30171>
[^12]: Niroula et al. arXiv 2609.04478, Sep 2026.
    <https://arxiv.org/abs/2609.04478>
[^13]: Revelio (SNP with dm-verity, VCEK fetch cost). Middleware'23.
    <https://arxiv.org/abs/2402.15277>
[^14]: A. Agache et al. "Firecracker: Lightweight Virtualization for Serverless
    Applications." NSDI'20.
    <https://www.usenix.org/conference/nsdi20/presentation/agache>
[^15]: S. Kuenzer et al. "Unikraft: Fast, Specialized Unikernels the Easy Way."
    EuroSys'21. <https://doi.org/10.1145/3447786.3456248>
[^16]: H.-C. Kuo et al. "A Linux in Unikernel Clothing" (Lupine). EuroSys'20.
    <https://doi.org/10.1145/3342195.3387526>
[^17]: NestedSGX (SGX-like enclaves inside SNP CVMs via VMPLs). NDSS'25.
    <https://arxiv.org/abs/2402.11438>
[^18]: D. Li et al. "Bifrost: Analysis and Optimization of Network I/O Tax in
    Confidential Virtual Machines." USENIX ATC'23.
    <https://www.usenix.org/conference/atc23/presentation/li-dingji>
[^19]: K. Shutemov, Linux unaccepted-memory work. <https://lwn.net/Articles/881010/>;
    per-vCPU accept throughput quoted in
    <https://www.mail-archive.com/kexec@lists.infradead.org/msg35388.html>
[^20]: dstack (TDX page accept and private-to-shared conversion measurements).
    <https://github.com/Dstack-TEE/dstack/pull/1220>
[^21]: QEMU documentation, AMD memory encryption (SEV, SEV-SNP,
    `kernel-hashes`, `host-data`).
    <https://www.qemu.org/docs/master/system/i386/amd-memory-encryption.html>
[^22]: QEMU documentation, Intel TDX.
    <https://www.qemu.org/docs/master/system/i386/tdx.html>
[^23]: QEMU source, `hw/i386/tdvf.c`.
    <https://gitlab.com/qemu-project/qemu/-/blob/master/hw/i386/tdvf.c>
[^24]: Cloud Hypervisor v52.0 release and PR #7942 (KVM SEV-SNP).
    <https://github.com/cloud-hypervisor/cloud-hypervisor/releases/tag/v52.0>,
    <https://github.com/cloud-hypervisor/cloud-hypervisor/pull/7942>
[^25]: Cloud Hypervisor PR #8123 (measured kernel/initrd/cmdline hashes, SNP ID
    block). <https://github.com/cloud-hypervisor/cloud-hypervisor/pull/8123>
[^26]: Cloud Hypervisor TDX issues #7181, #7207, #7630.
    <https://github.com/cloud-hypervisor/cloud-hypervisor/issues/7181>,
    <https://github.com/cloud-hypervisor/cloud-hypervisor/issues/7207>,
    <https://github.com/cloud-hypervisor/cloud-hypervisor/issues/7630>
[^27]: Cloud Hypervisor PR #8724 (closed) and the Cyberus Technology TDX
    re-enablement announcement, 2026-07-28.
    <https://github.com/cloud-hypervisor/cloud-hypervisor/pull/8724>;
    announcement in issue #6960:
    <https://github.com/cloud-hypervisor/cloud-hypervisor/issues/6960>
[^28]: Cloud Hypervisor issue #8454.
    <https://github.com/cloud-hypervisor/cloud-hypervisor/issues/8454>
[^29]: libkrun (libkrun-sev, libkrun-tdx; td-shim PRs #694 and #853).
    <https://github.com/libkrun>
[^30]: Firecracker. <https://github.com/firecracker-microvm/firecracker>;
    maintainer statement of 2025-06-05 in issue #2332:
    <https://github.com/firecracker-microvm/firecracker/issues/2332>
[^31]: Hyperlight issue #143.
    <https://github.com/hyperlight-dev/hyperlight/issues/143>
[^32]: Solo5. <https://github.com/Solo5/solo5>
[^33]: Kata Containers (kata-deploy 3.15.0).
    <https://github.com/kata-containers/kata-containers>
[^34]: Kata Containers issue #13535 (OVMF memory acceptance, TDX pod boot).
    <https://github.com/kata-containers/kata-containers/issues/13535>
[^35]: Kata Containers issue #13932 (`shared_fs=none`).
    <https://github.com/kata-containers/kata-containers/issues/13932>
[^36]: Confidential Containers project (v0.23, initdata).
    <https://github.com/confidential-containers>
[^37]: td-shim and issue #135 (SNP).
    <https://github.com/confidential-containers/td-shim>,
    <https://github.com/confidential-containers/td-shim/issues/135>
[^38]: Project Oak (stage0, restricted kernel).
    <https://github.com/project-oak/oak>
[^39]: EDK II / OVMF (TDVF, AmdSev). <https://github.com/tianocore/edk2>
[^40]: qboot. <https://github.com/bonzini/qboot>
[^41]: Unikraft PR #1094 (SEV/SEV-ES/SNP).
    <https://github.com/unikraft/unikraft/pull/1094>
[^42]: Unikraft issue #1528 (TDX).
    <https://github.com/unikraft/unikraft/issues/1528>
[^43]: hermit-sev organization (Hermit SNP kernel, Firecracker and
    rust-hypervisor-firmware forks). <https://github.com/hermit-sev>
[^44]: LiteBox. <https://github.com/microsoft/litebox>
[^45]: COCONUT-SVSM. <https://github.com/coconut-svsm/svsm>
[^46]: OpenVMM / OpenHCL. <https://github.com/microsoft/openvmm>
[^47]: IGVM format. <https://github.com/microsoft/igvm>
[^48]: Linux kernel source (`arch/x86/boot/compressed/mem.c`, SWIOTLB sizing,
    `tdx-guest` driver, configfs-tsm, KVM SNP/TDX host).
    <https://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git>
[^49]: Edgeless Contrast. <https://github.com/edgelesssys/contrast>
[^50]: AKS confidential containers (preview).
    <https://learn.microsoft.com/en-us/azure/aks/confidential-containers-overview>
[^51]: Confidential GKE nodes.
    <https://cloud.google.com/kubernetes-engine/docs/how-to/confidential-gke-nodes>
[^52]: Other guest projects surveyed: Enarx <https://github.com/enarx/enarx>,
    Mystikos <https://github.com/microsoft/mystikos>, Veracruz
    <https://github.com/veracruz-project/veracruz>, Occlum
    <https://github.com/occlum/occlum>, TamaGo
    <https://github.com/usbarmory/tamago>, Nanos
    <https://github.com/nanovms/nanos>, MirageOS
    <https://github.com/mirage/mirage>, OSv
    <https://github.com/cloudius-systems/osv>, Mewz
    <https://github.com/mewz-project/mewz>, Toro
    <https://github.com/torokernel/torokernel>.
[^53]: sev-snp-measure. <https://github.com/virtee/sev-snp-measure>
[^54]: Trustee (KBS). <https://github.com/confidential-containers/trustee>
[^55]: V. van Rijn, J. S. Rellermeyer. "A Fresh Look at the Architecture and
    Performance of Contemporary Isolation Platforms." Middleware'21.
    <https://arxiv.org/abs/2110.11462>
