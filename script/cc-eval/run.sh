#!/usr/bin/env bash
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
#
# run.sh: cold-boot measurement of a single-application Linux guest under
# urunc, across VMM / firmware paths. See README.md.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONF_DIR="/etc/urunc/cc-eval"
TS_LOG="/var/log/urunc/timestamps.log"
SHIM_DIR="/usr/local/bin"
MARKER="URUNC_GUEST_READY"
IMAGE_REPO="urunc/cc-eval-linux"

ALL_VARIANTS=(
	ch-direct
	fc-direct
	qemu-pc-seabios
	qemu-microvm-kernel
	qemu-microvm-stage0
	qemu-q35-ovmf-kernel
	qemu-microvm-stage0-snp
	qemu-q35-amdsev-snp
	qemu-q35-tdvf-tdx
)

ITER=20
VARIANTS_ARG=""
MEMORY_MB=256
OUT=""
TIMEOUT_S=30
SLEEP_S=1
NETWORK=""
WARMUP=1
ORDER="interleaved"

usage() {
	cat <<EOF
Usage: sudo $0 [options]

  -n, --iterations N   measured iterations per variant (default: $ITER)
  --variants a,b,c     variants to run (default: all available)
  --memory MB          guest RAM / container memory limit (default: $MEMORY_MB)
  --out DIR            results directory (default: results/<host>-<date>)
  --timeout S          seconds to wait for the guest marker (default: $TIMEOUT_S)
  --sleep S            pause between runs (default: $SLEEP_S)
  --network NAME       nerdctl network (default: nerdctl default, "none" to disable)
  --no-warmup          skip the uncounted warm-up run per variant
  --sequential         run all iterations of a variant before the next one
                       (default: interleaved rounds, which spreads host drift)
  -h, --help           this help

Variants: ${ALL_VARIANTS[*]}
TEE variants (*-snp, *-tdx) run only when the host exposes SEV-SNP / TDX.
EOF
}

log() { printf '[run %s] %s\n' "$(date +%H:%M:%S)" "$*"; }
warn() { printf '[run %s] WARNING: %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
die() {
	printf '[run] ERROR: %s\n' "$*" >&2
	exit 1
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	-n | --iterations)
		ITER="$2"
		shift 2
		;;
	--variants)
		VARIANTS_ARG="$2"
		shift 2
		;;
	--memory)
		MEMORY_MB="$2"
		shift 2
		;;
	--out)
		OUT="$2"
		shift 2
		;;
	--timeout)
		TIMEOUT_S="$2"
		shift 2
		;;
	--sleep)
		SLEEP_S="$2"
		shift 2
		;;
	--network)
		NETWORK="$2"
		shift 2
		;;
	--no-warmup)
		WARMUP=0
		shift
		;;
	--sequential)
		ORDER="sequential"
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		usage >&2
		die "unknown argument: $1"
		;;
	esac
done

[[ "$ITER" =~ ^[0-9]+$ ]] || die "-n must be a number"
[[ "$MEMORY_MB" =~ ^[0-9]+$ ]] || die "--memory must be a number (MB)"
[[ $EUID -eq 0 ]] || die "run as root (sudo $0 ...)"
command -v nerdctl >/dev/null || die "nerdctl not found; run setup.sh first"
command -v python3 >/dev/null || die "python3 not found"

HOSTNAME_SHORT="$(hostname -s 2>/dev/null || hostname)"
if [[ -z "$OUT" ]]; then
	OUT="$SCRIPT_DIR/results/${HOSTNAME_SHORT}-$(date +%Y%m%d-%H%M%S)"
fi
mkdir -p "$OUT/logs" "$OUT/ts" "$OUT/proc"
OUT="$(cd "$OUT" && pwd)"

HELPER="$(mktemp --suffix=.py /tmp/cc-eval-helper.XXXXXX)"
cleanup() {
	local names
	names="$(nerdctl ps -a --format '{{.Names}}' 2>/dev/null | grep '^cceval-' || true)"
	if [[ -n "$names" ]]; then
		# shellcheck disable=SC2086
		nerdctl rm -f $names >/dev/null 2>&1 || true
	fi
	rm -f "$HELPER"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# ---------------------------------------------------------------------------
# Python helper: host facts, per-iteration records, CSV export.
# ---------------------------------------------------------------------------
cat >"$HELPER" <<'PYEOF'
import calendar, csv, glob, json, os, re, subprocess, sys

TS_IDS = ["TS%02d" % i for i in range(19)]
LOG_RE = re.compile(r"^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(\.\d+)?(Z|[+-]\d\d:\d\d)\s?(.*)$")
PRINTK_RUN_RE = re.compile(r"\[\s*(\d+)\.(\d+)\]\s+Run (\S+) as init process")


def rfc3339_ns(date_s, frac, tz):
    import time
    secs = calendar.timegm(time.strptime(date_s, "%Y-%m-%dT%H:%M:%S"))
    if tz != "Z":
        sign = 1 if tz[0] == "+" else -1
        secs -= sign * (int(tz[1:3]) * 3600 + int(tz[4:6]) * 60)
    ns = 0
    if frac:
        digits = (frac[1:] + "000000000")[:9]
        ns = int(digits)
    return secs * 1000000000 + ns


def parse_log(path, marker):
    first = ready = run_host = run_printk = None
    if not os.path.exists(path):
        return first, ready, run_host, run_printk
    with open(path, errors="replace") as f:
        for line in f:
            m = LOG_RE.match(line.rstrip("\r\n"))
            if not m:
                continue
            t = rfc3339_ns(m.group(1), m.group(2), m.group(3))
            text = m.group(4).strip("\r")
            if first is None and text.strip():
                first = t
            if run_printk is None:
                pm = PRINTK_RUN_RE.search(text)
                if pm:
                    run_host = t
                    run_printk = int(pm.group(1)) * 1000000000 + \
                        int((pm.group(2) + "000000000")[:9])
            if ready is None and marker in text:
                ready = t
    return first, ready, run_host, run_printk


def parse_status(path):
    out = {}
    try:
        with open(path) as f:
            for line in f:
                k, _, v = line.partition(":")
                if k in ("VmRSS", "VmHWM"):
                    out[k] = int(v.split()[0])
    except OSError:
        pass
    return out


def parse_smaps_guest_rss(path, mem_mb):
    """Sum Rss of the mappings that look like guest RAM (size >= half of it)."""
    if not os.path.exists(path) or not mem_mb:
        return None
    thresh = mem_mb * 1024 // 2
    total = 0
    size = None
    with open(path) as f:
        for line in f:
            if re.match(r"^[0-9a-f]+-[0-9a-f]+ ", line):
                size = None
                continue
            if line.startswith("Size:"):
                size = int(line.split()[1])
            elif line.startswith("Rss:") and size is not None and size >= thresh:
                total += int(line.split()[1])
    return total


def cmd_record(a):
    ts = {}
    if os.path.exists(a["ts_file"]):
        with open(a["ts_file"]) as f:
            for line in f:
                try:
                    j = json.loads(line)
                except ValueError:
                    continue
                if a["cid"] and j.get("containerID") != a["cid"]:
                    continue
                tid = j.get("timestampID")
                if tid in TS_IDS and tid not in ts:
                    ts[tid] = int(j["time"])
    first, ready, run_host, run_printk = parse_log(a["log_file"], a["marker"])
    st = parse_status(a["status_file"])
    mem_mb = int(a["mem_mb"]) if a["mem_mb"] else 0
    guest_rss = parse_smaps_guest_rss(a["smaps_file"], mem_mb)
    rss = st.get("VmRSS")
    err = a["error"] or None
    ok = err is None and ready is not None and "TS00" in ts and "TS18" in ts
    if err is None and not ok:
        missing = [k for k in ("TS00", "TS18") if k not in ts]
        err = "incomplete record: missing %s" % (
            ", ".join(missing) if missing else "guest marker")
    rec = {
        "variant": a["variant"],
        "iter": int(a["iter"]),
        "warmup": a["warmup"] == "1",
        "container_id": a["cid"] or None,
        "ts": ts,
        "run_cmd_start_ns": int(a["start_ns"]) if a["start_ns"] else None,
        "run_cmd_end_ns": int(a["end_ns"]) if a["end_ns"] else None,
        "first_console_ns": first,
        "init_run_host_ns": run_host,
        "init_run_printk_ns": run_printk,
        "guest_ready_ns": ready,
        "vmm_pid": int(a["pid"]) if a["pid"] else None,
        "vmm_rss_kb": rss,
        "vmm_hwm_kb": st.get("VmHWM"),
        "guest_rss_kb": guest_rss,
        "vmm_overhead_rss_kb": (rss - guest_rss) if (rss is not None and guest_rss is not None) else None,
        "memory_mb": mem_mb or None,
        "rm_ms": float(a["rm_ms"]) if a["rm_ms"] else None,
        "ok": ok,
        "error": err,
    }
    with open(a["out"], "a") as f:
        f.write(json.dumps(rec, sort_keys=True) + "\n")
    print("ok" if ok else "fail: %s" % err)


def read(path):
    try:
        with open(path) as f:
            return f.read().strip()
    except OSError:
        return None


def run(cmd):
    try:
        return subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                              universal_newlines=True, timeout=20).stdout.strip()
    except Exception as e:  # noqa: BLE001
        return "error: %s" % e


def module_params(mod):
    d = {}
    for p in sorted(glob.glob("/sys/module/%s/parameters/*" % mod)):
        d[os.path.basename(p)] = read(p)
    return d


def cmd_host(a):
    lscpu = run(["lscpu"])
    model = None
    for line in lscpu.splitlines():
        if line.startswith("Model name:"):
            model = line.split(":", 1)[1].strip()
    os_name = None
    osr = read("/etc/os-release") or ""
    for line in osr.splitlines():
        if line.startswith("PRETTY_NAME="):
            os_name = line.split("=", 1)[1].strip('"')
    image_mb = None
    man = a.get("manifest")
    if man and os.path.exists(man):
        try:
            with open(man) as f:
                image_mb = json.load(f).get("image_mb")
        except ValueError:
            pass
    h = {
        "hostname": a["hostname"],
        "date": a["date"],
        "cpu_model": model,
        "nproc": os.cpu_count(),
        "lscpu": lscpu,
        "kernel": os.uname().release,
        "os": os_name,
        "qemu_version": run([a["qemu"], "--version"]).splitlines()[0] if a["qemu"] else None,
        "ch_version": run([a["ch"], "--version"]) if a["ch"] else None,
        "fc_version": run([a["fc"], "--version"]).splitlines()[0] if a["fc"] else None,
        "urunc_version": run(["urunc", "--version"]),
        "containerd_version": run(["containerd", "--version"]),
        "nerdctl_version": run(["nerdctl", "--version"]),
        "thp": read("/sys/kernel/mm/transparent_hugepage/enabled"),
        "thp_defrag": read("/sys/kernel/mm/transparent_hugepage/defrag"),
        "cpu_governor": read("/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor"),
        "kvm_params": module_params("kvm"),
        "kvm_amd_params": module_params("kvm_amd"),
        "kvm_intel_params": module_params("kvm_intel"),
        "dev_sev": os.path.exists("/dev/sev"),
        "snp": a["snp"] == "1",
        "tdx": a["tdx"] == "1",
        "memory_mb": int(a["memory_mb"]),
        "image_mb": image_mb,
        "iterations": int(a["iterations"]),
        "network": a["network"] or "default",
        "order": a["order"],
        "guest_build_manifest": json.load(open(man)) if man and os.path.exists(man) else None,
    }
    with open(a["out"], "w") as f:
        json.dump(h, f, indent=2, sort_keys=True)


def cmd_variants(a):
    out = {}
    for item in a["items"].split(";"):
        if not item:
            continue
        v, cfg, image, avail, reason = item.split("|", 4)
        out[v] = {
            "config_path": cfg,
            "config": read(cfg),
            "image": image,
            "runtime": "io.containerd.urunc-%s.v2" % v,
            "available": avail == "1",
            "skip_reason": reason or None,
        }
    with open(a["out"], "w") as f:
        json.dump(out, f, indent=2, sort_keys=True)


def cmd_csv(a):
    for path in sorted(glob.glob(os.path.join(a["dir"], "*.jsonl"))):
        if path.endswith(".warmup.jsonl"):
            continue
        rows = []
        with open(path) as f:
            for line in f:
                if line.strip():
                    rows.append(json.loads(line))
        cols = ["variant", "iter", "ok", "error", "container_id"] + TS_IDS + [
            "first_console_ns", "init_run_host_ns", "init_run_printk_ns",
            "guest_ready_ns", "vmm_rss_kb", "vmm_hwm_kb", "guest_rss_kb",
            "vmm_overhead_rss_kb", "memory_mb", "rm_ms"]
        with open(path[:-len(".jsonl")] + ".csv", "w", newline="") as f:
            w = csv.writer(f)
            w.writerow(cols)
            for r in rows:
                w.writerow([r.get("ts", {}).get(c) if c in TS_IDS else r.get(c) for c in cols])


def main():
    cmd = sys.argv[1]
    a = dict(arg.split("=", 1) for arg in sys.argv[2:])
    {"record": cmd_record, "host": cmd_host, "variants": cmd_variants,
     "csv": cmd_csv}[cmd](a)


main()
PYEOF

# ---------------------------------------------------------------------------
# Host capabilities and variant availability.
# ---------------------------------------------------------------------------
param_on() {
	local f="$1"
	[[ -r "$f" ]] && grep -qiE '^(y|1)' "$f"
}

HOST_SNP=0
HOST_TDX=0
if [[ -e /dev/sev ]] && param_on /sys/module/kvm_amd/parameters/sev_snp; then
	HOST_SNP=1
fi
if param_on /sys/module/kvm_intel/parameters/tdx; then
	HOST_TDX=1
fi
log "host: SEV-SNP=$HOST_SNP TDX=$HOST_TDX ($(nproc) CPUs)"

toml_get() {
	# toml_get FILE KEY: first string/number value of KEY in FILE.
	local file="$1" key="$2"
	sed -n -E "s/^[[:space:]]*${key}[[:space:]]*=[[:space:]]*\"?([^\"]*)\"?[[:space:]]*$/\1/p" "$file" | head -n1
}

image_for() {
	case "$1" in
	ch-*) echo "$IMAGE_REPO:cloud-hypervisor" ;;
	fc-*) echo "$IMAGE_REPO:firecracker" ;;
	*) echo "$IMAGE_REPO:qemu" ;;
	esac
}

qemu_at_least() {
	# qemu_at_least BIN MAJOR MINOR
	local ver maj min
	ver="$("$1" --version 2>/dev/null | head -n1 | grep -oE '[0-9]+\.[0-9]+(\.[0-9]+)?' | head -n1)"
	[[ -n "$ver" ]] || return 1
	maj="${ver%%.*}"
	min="${ver#*.}"
	min="${min%%.*}"
	((maj > $2 || (maj == $2 && min >= $3)))
}

SKIP_REASON=""
variant_available() {
	local v="$1" cfg mon fw tee
	cfg="$CONF_DIR/$v.toml"
	SKIP_REASON=""
	if [[ ! -f "$cfg" ]]; then
		SKIP_REASON="missing $cfg (run setup.sh)"
		return 1
	fi
	if [[ ! -x "$SHIM_DIR/containerd-shim-urunc-$v-v2" ]]; then
		SKIP_REASON="missing shim $SHIM_DIR/containerd-shim-urunc-$v-v2 (run setup.sh)"
		return 1
	fi
	mon="$(toml_get "$cfg" path)"
	if [[ -n "$mon" && ! -x "$mon" ]]; then
		SKIP_REASON="monitor binary $mon not found"
		return 1
	fi
	fw="$(toml_get "$cfg" firmware_path)"
	if [[ -n "$fw" && ! -s "$fw" ]]; then
		SKIP_REASON="firmware $fw not found"
		return 1
	fi
	tee="$(toml_get "$cfg" tee)"
	case "$tee" in
	sev-snp)
		if [[ $HOST_SNP -ne 1 ]]; then
			SKIP_REASON="host has no SEV-SNP (/dev/sev + kvm_amd.sev_snp=Y)"
			return 1
		fi
		if ! qemu_at_least "${mon:-qemu-system-x86_64}" 9 1; then
			SKIP_REASON="SEV-SNP needs QEMU >= 9.1"
			return 1
		fi
		;;
	tdx)
		if [[ $HOST_TDX -ne 1 ]]; then
			SKIP_REASON="host has no TDX (kvm_intel.tdx=Y)"
			return 1
		fi
		if ! qemu_at_least "${mon:-qemu-system-x86_64}" 10 1; then
			SKIP_REASON="TDX needs QEMU >= 10.1"
			return 1
		fi
		;;
	esac
	if ! nerdctl image inspect "$(image_for "$v")" >/dev/null 2>&1; then
		SKIP_REASON="image $(image_for "$v") not loaded (run build-guest.sh)"
		return 1
	fi
	return 0
}

if [[ -n "$VARIANTS_ARG" ]]; then
	IFS=',' read -r -a REQUESTED <<<"$VARIANTS_ARG"
else
	REQUESTED=("${ALL_VARIANTS[@]}")
fi

RUN_VARIANTS=()
VARIANT_ITEMS=""
for v in "${REQUESTED[@]}"; do
	known=0
	for k in "${ALL_VARIANTS[@]}"; do [[ "$k" == "$v" ]] && known=1; done
	[[ $known -eq 1 ]] || die "unknown variant '$v' (known: ${ALL_VARIANTS[*]})"
	if variant_available "$v"; then
		RUN_VARIANTS+=("$v")
		VARIANT_ITEMS+="$v|$CONF_DIR/$v.toml|$(image_for "$v")|1|;"
	else
		log "skipping $v: $SKIP_REASON"
		VARIANT_ITEMS+="$v|$CONF_DIR/$v.toml|$(image_for "$v")|0|$SKIP_REASON;"
	fi
done
[[ ${#RUN_VARIANTS[@]} -gt 0 ]] || die "no runnable variants"

python3 "$HELPER" variants out="$OUT/variants.json" items="$VARIANT_ITEMS"

QEMU_BIN="$(toml_get "$CONF_DIR/qemu-pc-seabios.toml" path 2>/dev/null || true)"
CH_BIN="$(toml_get "$CONF_DIR/ch-direct.toml" path 2>/dev/null || true)"
FC_BIN="$(toml_get "$CONF_DIR/fc-direct.toml" path 2>/dev/null || true)"
python3 "$HELPER" host out="$OUT/host.json" hostname="$HOSTNAME_SHORT" \
	date="$(date -Iseconds)" qemu="${QEMU_BIN:-$(command -v qemu-system-x86_64 || true)}" \
	ch="${CH_BIN:-$(command -v cloud-hypervisor || true)}" \
	fc="${FC_BIN:-$(command -v firecracker || true)}" \
	snp="$HOST_SNP" tdx="$HOST_TDX" memory_mb="$MEMORY_MB" iterations="$ITER" \
	network="$NETWORK" order="$ORDER" manifest="$SCRIPT_DIR/artifacts/manifest.json"

mkdir -p "$(dirname "$TS_LOG")"
touch "$TS_LOG"

log "results: $OUT"
log "variants: ${RUN_VARIANTS[*]}; iterations: $ITER; memory: ${MEMORY_MB} MB; order: $ORDER"

now_ns() { date +%s%N; }

# run_one VARIANT ITER WARMUP(0|1)
run_one() {
	local v="$1" i="$2" warm="$3"
	local name image runtime cid="" err="" pid="" start_ns end_ns rm_ms status
	local tag="$v-$i" outfile="$OUT/$v.jsonl" mem_for_rec="$MEMORY_MB"
	local -a extra=()

	if [[ "$warm" == "1" ]]; then
		tag="$v-warmup"
		outfile="$OUT/$v.warmup.jsonl"
	fi
	name="cceval-$tag"
	image="$(image_for "$v")"
	runtime="io.containerd.urunc-$v.v2"

	case "$v" in
	*-snp | *-tdx)
		# Guest RAM == container memory limit would OOM-kill the VMM once
		# all private memory is committed (F6). Use the config default and
		# no cgroup memory limit for the TEE variants.
		mem_for_rec="$(toml_get "$CONF_DIR/$v.toml" default_memory_mb)"
		;;
	*)
		extra+=(-m "${MEMORY_MB}m")
		;;
	esac
	if [[ -n "$NETWORK" ]]; then
		extra+=(--network "$NETWORK")
	fi

	nerdctl rm -f "$name" >/dev/null 2>&1 || true
	: >"$TS_LOG"
	rm -f "$OUT/proc/$tag.status" "$OUT/proc/$tag.smaps"

	start_ns="$(now_ns)"
	if ! cid="$(nerdctl run -d --name "$name" --runtime "$runtime" "${extra[@]}" "$image" 2>"$OUT/logs/$tag.err")"; then
		err="nerdctl run failed: $(tr '\n' ' ' <"$OUT/logs/$tag.err" | cut -c1-300)"
		cid=""
	fi
	end_ns="$(now_ns)"
	cid="$(printf '%s' "$cid" | tail -n1 | tr -d '[:space:]')"

	if [[ -z "$err" ]]; then
		local deadline=$((start_ns + TIMEOUT_S * 1000000000)) ready=0
		while :; do
			if nerdctl logs "$name" 2>/dev/null | grep -q "$MARKER"; then
				ready=1
				break
			fi
			status="$(nerdctl inspect -f '{{.State.Status}}' "$name" 2>/dev/null || echo unknown)"
			if [[ "$status" == "exited" || "$status" == "dead" ]]; then
				err="container $status before the guest marker"
				break
			fi
			if (($(now_ns) > deadline)); then
				err="timeout after ${TIMEOUT_S}s waiting for $MARKER"
				break
			fi
			sleep 0.05
		done
		pid="$(nerdctl inspect -f '{{.State.Pid}}' "$name" 2>/dev/null || true)"
		[[ "$pid" =~ ^[0-9]+$ && "$pid" != "0" ]] || pid=""
		if [[ $ready -eq 1 && -n "$pid" ]]; then
			cp "/proc/$pid/status" "$OUT/proc/$tag.status" 2>/dev/null || true
			cat "/proc/$pid/smaps" >"$OUT/proc/$tag.smaps" 2>/dev/null || true
		fi
		nerdctl logs -t "$name" >"$OUT/logs/$tag.log" 2>&1 || true
	fi

	local rm_start rm_end
	rm_start="$(now_ns)"
	nerdctl rm -f "$name" >/dev/null 2>&1 || true
	rm_end="$(now_ns)"
	rm_ms="$(awk -v a="$rm_start" -v b="$rm_end" 'BEGIN { printf "%.3f", (b - a) / 1e6 }')"

	if [[ -n "$cid" ]]; then
		grep -F "$cid" "$TS_LOG" >"$OUT/ts/$tag.log" 2>/dev/null || true
	else
		: >"$OUT/ts/$tag.log"
	fi
	[[ -s "$OUT/logs/$tag.err" ]] || rm -f "$OUT/logs/$tag.err"

	local res
	res="$(python3 "$HELPER" record out="$outfile" variant="$v" iter="$i" warmup="$warm" \
		cid="$cid" ts_file="$OUT/ts/$tag.log" log_file="$OUT/logs/$tag.log" \
		status_file="$OUT/proc/$tag.status" smaps_file="$OUT/proc/$tag.smaps" \
		start_ns="$start_ns" end_ns="$end_ns" rm_ms="$rm_ms" pid="$pid" \
		mem_mb="$mem_for_rec" marker="$MARKER" error="$err")"
	# Keep the (large) smaps only for the first iterations.
	if [[ "$i" =~ ^[0-9]+$ && "$i" -gt 3 ]]; then
		rm -f "$OUT/proc/$tag.smaps"
	fi
	log "$tag: $res"
	sleep "$SLEEP_S"
}

if [[ $WARMUP -eq 1 ]]; then
	for v in "${RUN_VARIANTS[@]}"; do
		run_one "$v" 0 1
	done
fi

if [[ "$ORDER" == "interleaved" ]]; then
	for ((i = 1; i <= ITER; i++)); do
		for v in "${RUN_VARIANTS[@]}"; do
			run_one "$v" "$i" 0
		done
	done
else
	for v in "${RUN_VARIANTS[@]}"; do
		for ((i = 1; i <= ITER; i++)); do
			run_one "$v" "$i" 0
		done
	done
fi

python3 "$HELPER" csv dir="$OUT"
log "done; analysing"
python3 "$SCRIPT_DIR/analyze.py" "$OUT" --quiet || warn "analyze.py failed"
log "results in $OUT (summary.md, summary.json, <variant>.jsonl/.csv)"
log "re-run the analysis with: ./analyze.py $OUT"
