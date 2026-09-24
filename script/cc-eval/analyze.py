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
"""Summarise a results directory produced by run.sh.

Usage: analyze.py RESULTS_DIR [--ram-mb N] [--image-mb N] [--quiet]

Reads RESULTS_DIR/<variant>.jsonl (one record per iteration), host.json and
variants.json, and writes RESULTS_DIR/summary.md and summary.json. Python
standard library only.
"""

import argparse
import glob
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import model  # noqa: E402

VARIANT_ORDER = [
    "fc-direct",
    "ch-direct",
    "qemu-microvm-kernel",
    "qemu-pc-seabios",
    "qemu-microvm-stage0",
    "qemu-q35-ovmf-kernel",
    "qemu-microvm-stage0-snp",
    "qemu-q35-amdsev-snp",
    "qemu-q35-tdvf-tdx",
]

# Phases computed per iteration (all in ms, rss in MB).
PHASES = [
    ("urunc", "urunc create+start (TS00->TS18)"),
    ("create", "urunc create (TS00->TS10)"),
    ("gap", "create->start gap (TS10->TS11)"),
    ("start", "urunc start->execve (TS11->TS18)"),
    ("boot", "execve->guest ready (TS18->marker)"),
    ("total", "total (TS00->marker)"),
    ("first_console", "execve->first console line"),
    ("pre_kernel", "execve->kernel clock zero (VMM+firmware+load)"),
    ("kernel", "guest kernel to init (printk)"),
    ("init_to_ready", "init->marker"),
    ("rss_mb", "VMM RSS at marker (MB)"),
    ("vmm_overhead_mb", "VMM RSS excl. guest RAM (MB)"),
    ("guest_rss_mb", "guest RAM resident (MB)"),
    ("rm_ms", "nerdctl rm -f (ms)"),
]

TEE_ANALOGUE = {
    "qemu-microvm-stage0-snp": "qemu-microvm-stage0",
    "qemu-q35-amdsev-snp": "qemu-q35-ovmf-kernel",
    "qemu-q35-tdvf-tdx": "qemu-q35-ovmf-kernel",
}


def ns_delta_ms(a, b):
    if a is None or b is None:
        return None
    return (a - b) / 1e6


def phases_of(rec):
    ts = rec.get("ts") or {}
    p = {}
    p["urunc"] = ns_delta_ms(ts.get("TS18"), ts.get("TS00"))
    p["create"] = ns_delta_ms(ts.get("TS10"), ts.get("TS00"))
    p["gap"] = ns_delta_ms(ts.get("TS11"), ts.get("TS10"))
    p["start"] = ns_delta_ms(ts.get("TS18"), ts.get("TS11"))
    ready = rec.get("guest_ready_ns")
    p["boot"] = ns_delta_ms(ready, ts.get("TS18"))
    p["total"] = ns_delta_ms(ready, ts.get("TS00"))
    p["first_console"] = ns_delta_ms(rec.get("first_console_ns"), ts.get("TS18"))
    run_host = rec.get("init_run_host_ns")
    run_printk = rec.get("init_run_printk_ns")
    if run_host is not None and run_printk is not None and ts.get("TS18"):
        p["pre_kernel"] = (run_host - run_printk - ts["TS18"]) / 1e6
        p["kernel"] = run_printk / 1e6
        p["init_to_ready"] = ns_delta_ms(ready, run_host)
    else:
        p["pre_kernel"] = p["kernel"] = p["init_to_ready"] = None
    for key, src in (("rss_mb", "vmm_rss_kb"),
                     ("vmm_overhead_mb", "vmm_overhead_rss_kb"),
                     ("guest_rss_mb", "guest_rss_kb")):
        v = rec.get(src)
        p[key] = v / 1024.0 if v is not None else None
    p["rm_ms"] = rec.get("rm_ms")
    return p


def percentile(sorted_vals, q):
    if not sorted_vals:
        return None
    if len(sorted_vals) == 1:
        return sorted_vals[0]
    pos = (len(sorted_vals) - 1) * q
    lo = int(pos)
    hi = min(lo + 1, len(sorted_vals) - 1)
    frac = pos - lo
    return sorted_vals[lo] * (1 - frac) + sorted_vals[hi] * frac


def stats(values):
    vals = sorted(v for v in values if v is not None)
    if not vals:
        return None
    return {
        "n": len(vals),
        "median": percentile(vals, 0.5),
        "p10": percentile(vals, 0.1),
        "p90": percentile(vals, 0.9),
        "min": vals[0],
        "max": vals[-1],
    }


def load_jsonl(path):
    recs = []
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                recs.append(json.loads(line))
            except ValueError:
                sys.stderr.write("warning: bad JSON line in %s\n" % path)
    return recs


def load_json(path):
    try:
        with open(path) as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


def fmt(v, digits=1):
    if v is None:
        return "-"
    return ("%%.%df" % digits) % v


def verdict(value, low, high):
    if value is None:
        return "n/a"
    if value < low:
        return "BELOW"
    if value > high:
        return "ABOVE"
    return "ok"


def med(summary, variant, phase):
    s = summary.get(variant, {}).get("phases", {}).get(phase)
    return s["median"] if s else None


def analyze(results_dir, ram_mb=None, image_mb=None):
    host = load_json(os.path.join(results_dir, "host.json"))
    variants_cfg = load_json(os.path.join(results_dir, "variants.json"))

    records = {}
    for path in sorted(glob.glob(os.path.join(results_dir, "*.jsonl"))):
        if path.endswith(".warmup.jsonl"):
            continue
        name = os.path.basename(path)[:-len(".jsonl")]
        records[name] = load_jsonl(path)

    order = [v for v in VARIANT_ORDER if v in records]
    order += sorted(v for v in records if v not in VARIANT_ORDER)

    summary = {}
    for v in order:
        recs = records[v]
        ok = [r for r in recs if r.get("ok")]
        errors = {}
        for r in recs:
            if not r.get("ok"):
                e = (r.get("error") or "unknown")[:120]
                errors[e] = errors.get(e, 0) + 1
        per_phase = {}
        all_p = [phases_of(r) for r in ok]
        for key, _ in PHASES:
            s = stats(p[key] for p in all_p)
            if s:
                per_phase[key] = s
        summary[v] = {"n_total": len(recs), "n_ok": len(ok),
                      "errors": errors, "phases": per_phase}

    # Predicted vs measured.
    checks = []
    for v in order:
        pred = model.PREDICTIONS.get(v)
        if pred:
            m = med(summary, v, "boot")
            checks.append({"variant": v, "phase": "boot", "measured": m,
                           "low": pred.low_ms, "high": pred.high_ms,
                           "verdict": verdict(m, pred.low_ms, pred.high_ms)})
        for phase in ("urunc", "kernel"):
            pp = model.PHASE_PREDICTIONS[phase]
            m = med(summary, v, phase)
            if m is None:
                continue
            checks.append({"variant": v, "phase": phase, "measured": m,
                           "low": pp.low_ms, "high": pp.high_ms,
                           "verdict": verdict(m, pp.low_ms, pp.high_ms)})
        if not v.endswith("-snp") and not v.endswith("-tdx"):
            rp = model.RSS_PREDICTIONS["qemu" if v.startswith("qemu") else "fc-ch"]
            m = med(summary, v, "vmm_overhead_mb")
            if m is not None:
                checks.append({"variant": v, "phase": "vmm_overhead_mb",
                               "measured": m, "low": rp.low_ms,
                               "high": rp.high_ms,
                               "verdict": verdict(m, rp.low_ms, rp.high_ms)})

    # Firmware-cost table.
    deltas = []
    pairs = []
    for v in order:
        if v.startswith("qemu-") and v != "qemu-microvm-kernel" \
                and v not in TEE_ANALOGUE:
            pairs.append((v, "qemu-microvm-kernel"))
    pairs.append(("ch-direct", "fc-direct"))
    pairs.append(("qemu-microvm-kernel", "fc-direct"))
    for tee, base in TEE_ANALOGUE.items():
        pairs.append((tee, base))
    for a, b in pairs:
        if a not in summary or b not in summary:
            continue
        row = {"variant": a, "reference": b}
        for phase in ("boot", "pre_kernel", "kernel", "total", "vmm_overhead_mb"):
            ma, mb = med(summary, a, phase), med(summary, b, phase)
            row[phase] = (ma - mb) if (ma is not None and mb is not None) else None
        dp = model.DELTA_PREDICTIONS.get((a, b))
        if dp:
            row["pred_low"], row["pred_high"] = dp.low_ms, dp.high_ms
            row["verdict"] = verdict(row["boot"], dp.low_ms, dp.high_ms)
        deltas.append(row)

    # Confidential estimate from the non-CC measurements.
    if ram_mb is None:
        ram_mb = host.get("memory_mb") or 256
    if image_mb is None:
        image_mb = host.get("image_mb") or 15
    estimate = None
    ref = "qemu-microvm-kernel"
    if ref in summary and summary[ref]["n_ok"]:
        boot = med(summary, ref, "boot")
        vmm = med(summary, ref, "pre_kernel")
        measured = {"urunc": med(summary, ref, "urunc"), "boot": boot}
        if vmm is not None and boot is not None:
            measured["vmm"] = vmm
            measured["guest"] = boot - vmm
        st_pre = med(summary, "qemu-microvm-stage0", "pre_kernel")
        st_boot = med(summary, "qemu-microvm-stage0", "boot")
        if st_pre is not None and vmm is not None:
            measured["stage0"] = max(0.0, st_pre - vmm)
        elif st_boot is not None and boot is not None:
            measured["stage0"] = max(0.0, st_boot - boot)
        est = model.estimate_confidential(measured, float(ram_mb), float(image_mb))
        for cfg, e in est.items():
            tv = model.ESTIMATE_VARIANT[cfg]
            mt = med(summary, tv, "total")
            e["variant"] = tv
            e["measured_total_ms"] = mt
            e["verdict"] = verdict(mt, e["low_ms"], e["high_ms"]) \
                if mt is not None else "not measured"
            pred = model.PREDICTIONS.get(tv)
            if pred:
                e["plan_boot_low_ms"] = pred.low_ms
                e["plan_boot_high_ms"] = pred.high_ms
        estimate = {"inputs_ms": measured, "ram_mb": ram_mb,
                    "image_mb": image_mb, "configs": est}

    return {"results_dir": os.path.abspath(results_dir), "host": host,
            "variants_config": sorted(variants_cfg.keys()) if isinstance(
                variants_cfg, dict) else [],
            "order": order, "summary": summary, "checks": checks,
            "firmware_deltas": deltas, "confidential_estimate": estimate}


def render_md(res):
    out = []
    host = res["host"]
    out.append("# urunc confidential-boot evaluation: summary")
    out.append("")
    out.append("<!-- cspell:disable -->")
    out.append("Results: `%s`" % res["results_dir"])
    out.append("")
    if host:
        out.append("Host: %s; CPU: %s; kernel %s; QEMU %s; CH %s; FC %s; "
                   "SNP=%s TDX=%s; THP=%s; guest RAM %s MB; image %s MB." % (
                       host.get("hostname", "?"), host.get("cpu_model", "?"),
                       host.get("kernel", "?"), host.get("qemu_version", "?"),
                       host.get("ch_version", "?"), host.get("fc_version", "?"),
                       host.get("snp", "?"), host.get("tdx", "?"),
                       host.get("thp", "?"), host.get("memory_mb", "?"),
                       host.get("image_mb", "?")))
        out.append("")
    if not res["order"]:
        out.append("No results found (no `<variant>.jsonl` files).")
        return "\n".join(out) + "\n"

    out.append("## Per-variant phases (median [p10, p90], ms)")
    out.append("")
    cols = ["urunc", "boot", "total", "pre_kernel", "kernel", "init_to_ready",
            "vmm_overhead_mb"]
    out.append("| Variant | ok/n | " + " | ".join(cols) + " |")
    out.append("|---|---|" + "---|" * len(cols))
    for v in res["order"]:
        s = res["summary"][v]
        cells = []
        for c in cols:
            st = s["phases"].get(c)
            if st:
                cells.append("%s [%s, %s]" % (fmt(st["median"]),
                                              fmt(st["p10"]), fmt(st["p90"])))
            else:
                cells.append("-")
        out.append("| `%s` | %d/%d | %s |" % (v, s["n_ok"], s["n_total"],
                                             " | ".join(cells)))
    out.append("")
    out.append("Phase legend: " + "; ".join(
        "`%s` = %s" % (k, d) for k, d in PHASES))
    out.append("")
    errs = [(v, s["errors"]) for v, s in res["summary"].items() if s["errors"]]
    if errs:
        out.append("### Failed iterations")
        out.append("")
        for v, e in errs:
            for msg, n in e.items():
                out.append("- `%s`: %d x %s" % (v, n, msg))
        out.append("")

    out.append("## Predicted vs measured (medians)")
    out.append("")
    out.append("| Variant | Phase | Measured | Predicted | Verdict |")
    out.append("|---|---|---:|---|---|")
    for c in res["checks"]:
        out.append("| `%s` | %s | %s | %g-%g | %s |" % (
            c["variant"], c["phase"], fmt(c["measured"]), c["low"], c["high"],
            c["verdict"] if c["verdict"] == "ok" else "**%s**" % c["verdict"]))
    out.append("")

    if res["firmware_deltas"]:
        out.append("## Firmware / boot-path cost (difference of medians, ms)")
        out.append("")
        out.append("| Variant | minus | boot | pre_kernel | kernel | total | "
                   "VMM RSS (MB) | Predicted boot delta | Verdict |")
        out.append("|---|---|---:|---:|---:|---:|---:|---|---|")
        for d in res["firmware_deltas"]:
            pred = "%g-%g" % (d["pred_low"], d["pred_high"]) \
                if "pred_low" in d else "-"
            out.append("| `%s` | `%s` | %s | %s | %s | %s | %s | %s | %s |" % (
                d["variant"], d["reference"], fmt(d["boot"]),
                fmt(d["pre_kernel"]), fmt(d["kernel"]), fmt(d["total"]),
                fmt(d["vmm_overhead_mb"]), pred, d.get("verdict", "-")))
        out.append("")

    est = res["confidential_estimate"]
    out.append("## Bottom-up confidential estimate (total, ms)")
    out.append("")
    if not est:
        out.append("Needs `qemu-microvm-kernel` results.")
    else:
        out.append("Inputs (medians of `qemu-microvm-kernel`, ms): %s; RAM %s MB; "
                   "kernel+initrd %s MB." % (
                       ", ".join("%s=%s" % (k, fmt(v))
                                 for k, v in est["inputs_ms"].items()),
                       est["ram_mb"], est["image_mb"]))
        out.append("")
        out.append("| Configuration | Variant | Estimate low | Estimate high | "
                   "Plan boot range | Measured total | Verdict |")
        out.append("|---|---|---:|---:|---|---:|---|")
        for cfg, e in est["configs"].items():
            plan = "%d-%d" % (e["plan_boot_low_ms"], e["plan_boot_high_ms"]) \
                if "plan_boot_low_ms" in e else "-"
            out.append("| %s | `%s` | %.0f | %.0f | %s | %s | %s |" % (
                cfg, e["variant"], e["low_ms"], e["high_ms"], plan,
                fmt(e["measured_total_ms"]), e["verdict"]))
        out.append("")
        for cfg, e in est["configs"].items():
            out.append("<details><summary>%s terms</summary>" % cfg)
            out.append("")
            out.append("| Term | Low (ms) | High (ms) |")
            out.append("|---|---:|---:|")
            for name, lo, hi in e["terms"]:
                out.append("| %s | %.1f | %.1f |" % (name, lo, hi))
            out.append("")
            out.append("</details>")
            out.append("")
        out.append("Constants and sources: see `predictions.md` / `model.py`.")
    out.append("")
    return "\n".join(out)


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("results_dir")
    ap.add_argument("--ram-mb", type=float, default=None,
                    help="guest RAM for the confidential estimate "
                         "(default: host.json memory_mb or 256)")
    ap.add_argument("--image-mb", type=float, default=None,
                    help="kernel+initrd size for the estimate "
                         "(default: host.json image_mb or 15)")
    ap.add_argument("--quiet", action="store_true",
                    help="do not print summary.md to stdout")
    args = ap.parse_args()
    if not os.path.isdir(args.results_dir):
        sys.stderr.write("error: %s is not a directory\n" % args.results_dir)
        return 2
    res = analyze(args.results_dir, args.ram_mb, args.image_mb)
    md = render_md(res)
    with open(os.path.join(args.results_dir, "summary.md"), "w") as f:
        f.write(md)
    with open(os.path.join(args.results_dir, "summary.json"), "w") as f:
        json.dump(res, f, indent=2, sort_keys=True, default=str)
    if not args.quiet:
        print(md)
    sys.stderr.write("wrote %s and summary.json\n" % os.path.join(
        args.results_dir, "summary.md"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
