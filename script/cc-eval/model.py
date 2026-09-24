#!/usr/bin/env python3
# Copyright (c) 2023-2026, Nubificus LTD
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# cspell:disable
"""Boot-latency model for the urunc confidential-boot evaluation.

The numbers here are the "Predicted evaluation results" of the feasibility
study (docs/design/confidential.md). They describe a 1 vCPU, 128-256 MB guest
with a ~10-20 MB kernel+initrd on a modern x86 server.

Tags: [M] measured in the cited work, [D] derived from measured numbers,
[E] estimate.

Run `python3 model.py` to print the table, or `python3 model.py --write` to
regenerate predictions.md next to this file.

Only the Python standard library is used.
"""

from collections import namedtuple
import os
import sys

Prediction = namedtuple(
    "Prediction", ["low_ms", "high_ms", "rationale", "sources", "phase"])
Constant = namedtuple(
    "Constant", ["low", "high", "unit", "tag", "source"])

# Phase names used by analyze.py:
#   urunc  = TS00 -> TS18 (urunc create + start until execve of the VMM)
#   boot   = TS18 -> guest marker (VMM exec, firmware, kernel, init, app)
#   total  = TS00 -> guest marker
#   kernel = guest printk time of "Run <init> as init process"
#   rss    = VMM resident memory at the marker, excluding guest RAM (MB)

S_SEVERIFAST = "SEVeriFast, ASPLOS'24"
S_NSDI20 = "Firecracker, NSDI'20"
S_UNIKRAFT = "Unikraft, EuroSys'21"
S_MISONO = "Misono et al., SIGMETRICS'25"
S_TUM = "TUM 'Confidential VMs Explained', SIGMETRICS'25"
S_TSDOC = "urunc docs/developer-guide/timestamps.md"
S_F6 = "feasibility study F6 (adversarial review)"
S_WALLET = "Wallet, NSDI'26"
S_COFUNC = "CoFunc, USENIX Security'25"

# Per-variant prediction of the boot phase (TS18 -> guest marker).
PREDICTIONS = {
    "fc-direct": Prediction(
        40, 150,
        "Firecracker direct vmlinux boot: 28-47 ms with a Lupine/AWS kernel, "
        "~100-150 ms with a generic kernel.",
        [S_SEVERIFAST, S_NSDI20], "boot"),
    "ch-direct": Prediction(
        60, 180,
        "Same kernel boot as Firecracker; the Cloud Hypervisor process is "
        "~10-30 ms slower to set up.",
        [S_NSDI20, "Cloud Hypervisor ~100 ms boot (survey)"], "boot"),
    "qemu-microvm-kernel": Prediction(
        80, 200,
        "microvm adds ~6 ms over Firecracker for a 2 MB image; QEMU process "
        "start is 30-80 ms.",
        [S_UNIKRAFT], "boot"),
    "qemu-pc-seabios": Prediction(
        150, 350,
        "SeaBIOS + PCI enumeration + i440fx; pc is ~29 ms slower than "
        "microvm for a tiny guest, far more for Linux with PCI.",
        [S_UNIKRAFT], "boot"),
    "qemu-microvm-stage0": Prediction(
        80 + 30, 200 + 120,
        "qemu-microvm-kernel + 30-120 ms: fw_cfg DMA copy of 10-20 MB, "
        "stage0 init, e820/ACPI build; non-CC so no exit per 4 KB.",
        ["Oak stage0 source", S_F6], "boot"),
    "qemu-q35-ovmf-kernel": Prediction(
        80 + 300, 200 + 900,
        "qemu-microvm-kernel + 300-900 ms: OVMF SEC/PEI/DXE/BDS on plain KVM "
        "is 0.3-0.9 s for direct kernel boot (Misono: 0.75-0.92 s firmware "
        "phase for a normal VM).",
        [S_MISONO, S_TUM], "boot"),
    # TEE host only.
    "qemu-microvm-stage0-snp": Prediction(
        350, 800,
        "Stage0 under SEV-SNP: PSP measurement of the ROM, eager PVALIDATE of "
        "all RAM, fw_cfg through one shared bounce page, Linux ~2.3x slower. "
        ">1 s if a 2 MiB ROM is measured whole (the pinned stage0 ROM is "
        "512 KiB, i.e. ~125 ms of PSP time).",
        [S_SEVERIFAST, S_F6], "boot"),
    "qemu-q35-amdsev-snp": Prediction(
        1500, 3500,
        "QEMU + OVMF AmdSev: pre-encrypt ~0.3 s plus 1.1-3.2 s of firmware "
        "under SNP.",
        [S_SEVERIFAST, S_TUM], "boot"),
    "qemu-q35-tdvf-tdx": Prediction(
        700, 1800,
        "TDVF at 256 MB: TDVF DXE 0.3-0.5 s, eager accept below 512 MB, "
        "fw_cfg kernel load measured into RTMR[1], Linux TD slowdown.",
        [S_TUM, S_F6], "boot"),
}

# Predictions that apply to every variant (or to a group of them).
PHASE_PREDICTIONS = {
    "urunc": Prediction(
        5, 30,
        "urunc create+start up to execve of the VMM: the current numbers in "
        "timestamps.md are ~1-12 ms on the create side; disk/initrd mutation "
        "dominates with an initrd rootfs.",
        [S_TSDOC], "urunc"),
    "kernel": Prediction(
        60, 150,
        "Guest Linux boot to the init process: 60-150 ms for a tuned kernel, "
        "200-400 ms for a distro-like config.",
        [S_NSDI20, S_SEVERIFAST], "kernel"),
}

# VMM resident memory at marker time, guest RAM excluded (MB).
RSS_PREDICTIONS = {
    "qemu": Prediction(
        15, 40, "QEMU process overhead on top of guest RAM.",
        [S_COFUNC], "rss"),
    "fc-ch": Prediction(
        5, 15, "Firecracker/Cloud Hypervisor overhead on top of guest RAM.",
        [S_COFUNC, S_NSDI20], "rss"),
}

# Firmware/boot-path cost relative to a reference variant (boot phase).
DELTA_PREDICTIONS = {
    ("qemu-microvm-stage0", "qemu-microvm-kernel"): Prediction(
        30, 120, "Oak stage0 + fw_cfg image copy (non-CC).",
        [S_F6], "boot"),
    ("qemu-q35-ovmf-kernel", "qemu-microvm-kernel"): Prediction(
        300, 900, "OVMF SEC/PEI/DXE/BDS on plain KVM.",
        [S_MISONO, S_TUM], "boot"),
    ("qemu-pc-seabios", "qemu-microvm-kernel"): Prediction(
        20, 200,
        "[D] from the two absolute ranges: SeaBIOS + PCI enumeration; "
        "~29 ms for a tiny guest, more for Linux with PCI.",
        [S_UNIKRAFT], "boot"),
    ("ch-direct", "fc-direct"): Prediction(
        10, 30, "Cloud Hypervisor process setup vs Firecracker.",
        [S_NSDI20], "boot"),
}

# Per-unit confidential-computing costs used by estimate_confidential().
CONSTANTS = {
    "snp_psp_mb_per_s": Constant(
        3.9, 4.2, "MB/s", "D",
        "SEVeriFast ASPLOS'24: SNP_LAUNCH_UPDATE 23 MB vmlinux = 5.65 s, "
        "12 MB initrd = 2.85 s, 1 MB OVMF = 257 ms (~4 MB/s on Milan)"),
    "snp_pvalidate_us_per_4k": Constant(
        1.5, 5.0, "us/4K page", "E",
        "PVALIDATE + guest_memfd fault + RMPUPDATE, eager (stage0, OVMF "
        "below its accept threshold); Wallet NSDI'26 measured 24 us/page "
        "worst case"),
    "fwcfg_cvm_ms_per_mb": Constant(
        5.0, 15.0, "ms/MB", "E",
        "Oak stage0 fw_cfg through one 4 KiB shared bounce page under a CVM: "
        "port-I/O exits (#VC/#VE -> userspace, 8-20 us each) + copy + SHA-256"),
    "tdx_accept_us_per_4k": Constant(
        3.0, 10.0, "us/4K page", "D/E",
        "TDH.MEM.PAGE.AUG + TDG.MEM.PAGE.ACCEPT, 2.5-8 us/page measured "
        "lazily (Shutemov, dstack) plus fault overhead when eager"),
    "tdx_page_add_ms_per_mb": Constant(
        5.0, 10.0, "ms/MB", "E",
        "TDH.MEM.PAGE.ADD + 16x TDH.MR.EXTEND per 4K page for the initial "
        "(measured) TDVF image"),
    "linux_cvm_slowdown": Constant(
        2.3, 2.3, "x", "M",
        "SEVeriFast ASPLOS'24: Linux boot under SNP ~2.3x slower; applied "
        "to TDX as an estimate"),
    "tdvf_dxe_s": Constant(
        0.3, 0.5, "s", "E",
        "TDVF SEC/PEI/DXE/BDS with lazy accept, small TD (TUM Fig. 4: "
        "1.05 s at 8 GB)"),
    "ovmf_snp_s": Constant(
        1.1, 3.2, "s", "M",
        "OVMF AmdSev firmware phase under SNP (TUM SIGMETRICS'25 Fig. 4 "
        "3.81 s at 8 GB; SEVeriFast 3.2 s; CoFunc/Segarra DXE ~3 s under SEV)"),
    "attestation_ms": Constant(
        3.0, 12.0, "ms", "M",
        "SNP report 6 ms (Wallet: 5.88 ms); TDX report 0.02 ms + quote "
        "3-7 ms; verification 1-3 ms; cached collateral"),
}

# Firmware image sizes used for the measurement terms (MB).
FIRMWARE_MB = {
    "stage0": 0.5,      # pinned Oak stage0_bin is 512 KiB (BIOS_SIZE=512K)
    "ovmf_amdsev": 4.0,  # OVMF.amdsev.fd, ~4 MB
    "tdvf": 4.0,        # OVMF.inteltdx.fd / TDVF, ~4 MB
}


def _get(measured, key, default=0.0):
    val = measured.get(key)
    if val is None:
        return default
    return float(val)


def _const(name, which):
    c = CONSTANTS[name]
    return c.low if which == "low" else c.high


def estimate_confidential(measured_non_cc_phases_ms, ram_mb, image_mb):
    """Bottom-up estimate of the confidential boot from non-CC measurements.

    measured_non_cc_phases_ms is a dict (median values in ms) with:
      urunc        TS00 -> TS18, any variant (CC does not change it)
      vmm          pre-kernel time of qemu-microvm-kernel (VMM start +
                   minimal firmware + kernel load), from the printk anchor
      stage0       extra pre-kernel time of qemu-microvm-stage0 over
                   qemu-microvm-kernel (non-CC stage0 + fw_cfg copy)
      guest        kernel + init + app up to the marker (TS18 -> marker
                   minus vmm), from the reference variant
    Missing keys count as 0, except guest which falls back to boot.

    Returns {config: {"low_ms", "high_ms", "terms": [(name, low, high)]}}
    for "snp-stage0", "snp-ovmf" and "tdx-tdvf".
    """
    m = measured_non_cc_phases_ms
    urunc = _get(m, "urunc")
    vmm = _get(m, "vmm")
    stage0 = _get(m, "stage0")
    guest = _get(m, "guest", _get(m, "boot"))
    pages = ram_mb * 256.0

    def build(terms):
        low = sum(t[1] for t in terms)
        high = sum(t[2] for t in terms)
        return {"low_ms": low, "high_ms": high, "terms": terms}

    def slowdown(which):
        return guest * _const("linux_cvm_slowdown", which)

    def psp(mb, which):
        # Lower throughput means higher latency, hence the swap.
        rate = _const("snp_psp_mb_per_s", "high" if which == "low" else "low")
        return mb / rate * 1000.0

    common = [
        ("urunc TS00->TS18 (measured)", urunc, urunc),
        ("VMM start, no firmware (measured)", vmm, vmm),
    ]
    att = ("attestation report + verify",
           _const("attestation_ms", "low"), _const("attestation_ms", "high"))
    fwcfg = ("fw_cfg kernel+initrd under CVM (%.1f MB)" % image_mb,
             image_mb * _const("fwcfg_cvm_ms_per_mb", "low"),
             image_mb * _const("fwcfg_cvm_ms_per_mb", "high"))
    guest_t = ("Linux + init under CVM (measured x slowdown)",
               slowdown("low"), slowdown("high"))

    snp_stage0 = common + [
        ("stage0 init (measured non-CC delta)", stage0, stage0),
        ("PSP measure of stage0 ROM (%.1f MB)" % FIRMWARE_MB["stage0"],
         psp(FIRMWARE_MB["stage0"], "low"), psp(FIRMWARE_MB["stage0"], "high")),
        ("eager PVALIDATE of %d MB" % ram_mb,
         pages * _const("snp_pvalidate_us_per_4k", "low") / 1000.0,
         pages * _const("snp_pvalidate_us_per_4k", "high") / 1000.0),
        fwcfg, guest_t, att,
    ]
    snp_ovmf = common + [
        ("PSP measure of OVMF AmdSev (%.1f MB)" % FIRMWARE_MB["ovmf_amdsev"],
         psp(FIRMWARE_MB["ovmf_amdsev"], "low"),
         psp(FIRMWARE_MB["ovmf_amdsev"], "high")),
        ("OVMF AmdSev firmware under SNP",
         _const("ovmf_snp_s", "low") * 1000.0,
         _const("ovmf_snp_s", "high") * 1000.0),
        ("eager PVALIDATE of %d MB" % ram_mb,
         pages * _const("snp_pvalidate_us_per_4k", "low") / 1000.0,
         pages * _const("snp_pvalidate_us_per_4k", "high") / 1000.0),
        fwcfg, guest_t, att,
    ]
    tdx_tdvf = common + [
        ("TDH.MEM.PAGE.ADD+MR.EXTEND of TDVF (%.1f MB)" % FIRMWARE_MB["tdvf"],
         FIRMWARE_MB["tdvf"] * _const("tdx_page_add_ms_per_mb", "low"),
         FIRMWARE_MB["tdvf"] * _const("tdx_page_add_ms_per_mb", "high")),
        ("TDVF DXE", _const("tdvf_dxe_s", "low") * 1000.0,
         _const("tdvf_dxe_s", "high") * 1000.0),
        ("accept of %d MB (TDVF pre-accepts up to 512 MB)" % ram_mb,
         min(ram_mb, 512) * 256.0 * _const("tdx_accept_us_per_4k", "low") / 1000.0,
         min(ram_mb, 512) * 256.0 * _const("tdx_accept_us_per_4k", "high") / 1000.0),
        fwcfg, guest_t, att,
    ]
    return {
        "snp-stage0": build(snp_stage0),
        "snp-ovmf": build(snp_ovmf),
        "tdx-tdvf": build(tdx_tdvf),
    }


# Which TEE variant each estimate corresponds to.
ESTIMATE_VARIANT = {
    "snp-stage0": "qemu-microvm-stage0-snp",
    "snp-ovmf": "qemu-q35-amdsev-snp",
    "tdx-tdvf": "qemu-q35-tdvf-tdx",
}


def predictions_markdown():
    out = []
    out.append("# Predicted evaluation results")
    out.append("")
    out.append("<!-- cspell:disable -->")
    out.append("Generated by `python3 model.py --write` from `model.py`; keep "
               "them in sync. Scope: 1 vCPU, 128-256 MB guest, ~10-20 MB "
               "kernel+initrd, modern x86 server. Tags: [M] measured, "
               "[D] derived, [E] estimate.")
    out.append("")
    out.append("## Boot phase per variant (TS18 -> guest marker)")
    out.append("")
    out.append("| Variant | Low (ms) | High (ms) | Rationale | Sources |")
    out.append("|---|---:|---:|---|---|")
    for name, p in PREDICTIONS.items():
        out.append("| `%s` | %d | %d | %s | %s |" % (
            name, p.low_ms, p.high_ms, p.rationale, "; ".join(p.sources)))
    out.append("")
    out.append("TEE variants (`*-snp`, `*-tdx`) run only on a host that "
               "exposes SEV-SNP or TDX.")
    out.append("")
    out.append("## Phases common to all variants")
    out.append("")
    out.append("| Phase | Low (ms) | High (ms) | Rationale | Sources |")
    out.append("|---|---:|---:|---|---|")
    for name, p in PHASE_PREDICTIONS.items():
        out.append("| %s | %d | %d | %s | %s |" % (
            name, p.low_ms, p.high_ms, p.rationale, "; ".join(p.sources)))
    out.append("")
    out.append("## Firmware / boot-path deltas (boot phase)")
    out.append("")
    out.append("| Variant minus reference | Low (ms) | High (ms) | Rationale |")
    out.append("|---|---:|---:|---|")
    for (a, b), p in DELTA_PREDICTIONS.items():
        out.append("| `%s` - `%s` | %d | %d | %s |" % (
            a, b, p.low_ms, p.high_ms, p.rationale))
    out.append("")
    out.append("## VMM memory (RSS at marker, guest RAM excluded)")
    out.append("")
    out.append("| VMM | Low (MB) | High (MB) | Rationale |")
    out.append("|---|---:|---:|---|")
    for name, p in RSS_PREDICTIONS.items():
        out.append("| %s | %d | %d | %s |" % (
            name, p.low_ms, p.high_ms, p.rationale))
    out.append("")
    out.append("Guest RAM is fully resident only in the TEE variants "
               "(eager validation/acceptance, no page sharing).")
    out.append("")
    out.append("## Constants for the confidential estimate")
    out.append("")
    out.append("| Constant | Low | High | Unit | Tag | Source |")
    out.append("|---|---:|---:|---|---|---|")
    for name, c in CONSTANTS.items():
        out.append("| `%s` | %g | %g | %s | [%s] | %s |" % (
            name, c.low, c.high, c.unit, c.tag, c.source))
    out.append("")
    out.append("`analyze.py` adds these per-unit costs to the measured "
               "non-CC phases (`estimate_confidential(measured, ram_mb, "
               "image_mb)`) and prints best/worst bounds for the SNP-stage0, "
               "SNP-OVMF and TDX-TDVF configurations.")
    out.append("")
    ex = estimate_confidential(
        {"urunc": 15, "vmm": 60, "stage0": 60, "guest": 100}, 256, 15)
    out.append("Example with urunc=15 ms, VMM=60 ms, stage0=60 ms, "
               "guest=100 ms, 256 MB RAM, 15 MB image:")
    out.append("")
    out.append("| Configuration | Low (ms) | High (ms) |")
    out.append("|---|---:|---:|")
    for name, e in ex.items():
        out.append("| %s | %.0f | %.0f |" % (name, e["low_ms"], e["high_ms"]))
    out.append("")
    return "\n".join(out)


def main():
    text = predictions_markdown()
    if len(sys.argv) > 1 and sys.argv[1] == "--write":
        path = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                            "predictions.md")
        with open(path, "w") as f:
            f.write(text)
        print("wrote %s" % path)
    else:
        print(text)


if __name__ == "__main__":
    main()
