#!/usr/bin/env bash
# run-benchmarks.sh — Runs the ix benchmark suite and prints a comparison table.
#
# Requirements:
#   IX_ROOTFS_IMAGE — path to ext4 rootfs image  (default: /opt/ix/rootfs/base.ext4)
#   IX_KERNEL_PATH  — path to vmlinux kernel     (default: /opt/ix/firecracker/vmlinux.bin)
#   IX_FC_BINARY    — path to firecracker binary (default: searches PATH)
#
# Usage:
#   ./scripts/run-benchmarks.sh [iterations]
#
# Examples:
#   ./scripts/run-benchmarks.sh          # 5 iterations per benchmark (default)
#   ./scripts/run-benchmarks.sh 10       # 10 iterations per benchmark
#   IX_ROOTFS=/my/rootfs ./scripts/run-benchmarks.sh

set -euo pipefail

# Compare two saved runs: ./scripts/run-benchmarks.sh compare old.txt new.txt
if [[ "${1:-}" == "compare" ]]; then
    if [[ $# -lt 3 ]]; then
        echo "usage: $0 compare <old.txt> <new.txt>" >&2
        exit 1
    fi
    command -v benchstat >/dev/null || {
        echo "benchstat not found: go install golang.org/x/perf/cmd/benchstat@latest" >&2
        exit 1
    }
    exec benchstat "$2" "$3"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

ITERATIONS="${1:-5}"

# COUNT > 1 repeats every benchmark for benchstat-grade variance data.
COUNT="${COUNT:-1}"
RESULTS_DIR="${REPO_ROOT}/bench-results"
mkdir -p "${RESULTS_DIR}"
OUT_FILE="${RESULTS_DIR}/bench-$(date +%Y%m%d-%H%M%S).txt"

# ── Docker baselines from the spec (ms) ───────────────────────────────────────
DOCKER_CREATE=368
DOCKER_SHELL=42
DOCKER_FILE=45
DOCKER_CODE_WARM=53
DOCKER_FIRST_CODE=2750
DOCKER_E2E=393

# ── Spec targets (ms) ─────────────────────────────────────────────────────────
TARGET_CREATE_COLD=100
TARGET_CREATE_POOL=1
TARGET_SHELL_PERSISTENT=3
TARGET_SHELL_ONESHOT=12
TARGET_FILE=6
TARGET_CODE_WARM=10
TARGET_FIRST_CODE=300
TARGET_FIRST_CODE_PREWARM=10
TARGET_E2E=25

# ── Colours ───────────────────────────────────────────────────────────────────
RED='\033[0;31m'
GRN='\033[0;32m'
YLW='\033[0;33m'
RST='\033[0m'

# ── Run benchmarks ────────────────────────────────────────────────────────────
echo ""
echo "Running ix benchmark suite (${ITERATIONS} iterations per benchmark)..."
echo "  IX_ROOTFS_IMAGE = ${IX_ROOTFS_IMAGE:-/opt/ix/rootfs/base.ext4}"
echo "  IX_KERNEL_PATH  = ${IX_KERNEL_PATH:-/opt/ix/firecracker/vmlinux.bin}"
echo "  IX_FC_BINARY    = ${IX_FC_BINARY:-(PATH lookup)}"
echo ""

set +e
cd "${REPO_ROOT}" && go test \
    -bench=. \
    -benchtime="${ITERATIONS}x" \
    -tags=integration \
    -count="${COUNT}" \
    -timeout=60m \
    2>&1 | tee "${OUT_FILE}"
bench_status=${PIPESTATUS[0]}
set -e

RAW_OUTPUT=$(cat "${OUT_FILE}")

echo ""
echo "Raw results saved to: ${OUT_FILE}  (compare runs with: $0 compare old.txt new.txt)"
echo ""

# ── Parse ns/op values ────────────────────────────────────────────────────────
# grep a benchmark name and return its ns/op value (or "N/A" if not found).
# Multiple lines (COUNT>1) are averaged.
get_ns() {
    local name="$1"
    local ns
    ns=$(printf '%s\n' "${RAW_OUTPUT}" \
        | grep -E "^${name}[^0-9a-zA-Z]" \
        | awk '{for(i=1;i<=NF;i++) if($(i+1)=="ns/op") {sum+=$i; n++}} END {if(n) printf "%d", sum/n}')
    printf '%s' "${ns:-N/A}"
}

ns_create_cold=$(get_ns "BenchmarkCreateCold")
ns_create_pool=$(get_ns "BenchmarkCreateFromPool")
ns_shell_persistent=$(get_ns "BenchmarkShellPersistent")
ns_shell_oneshot=$(get_ns "BenchmarkShellOneShot")
ns_file=$(get_ns "BenchmarkFileReadWrite")
ns_code_warm=$(get_ns "BenchmarkCodeExecPython")
ns_first_code=$(get_ns "BenchmarkCodeExecFirstCall")
ns_first_prewarm=$(get_ns "BenchmarkCreatePoolPreWarmed")
ns_e2e=$(get_ns "BenchmarkE2EAgentCycle")

# ── Convert ns → ms (2 decimal places) ───────────────────────────────────────
ns_to_ms() {
    local ns="$1"
    if [[ "${ns}" == "N/A" ]]; then
        printf "N/A"
        return
    fi
    awk "BEGIN { printf \"%.2f\", ${ns}/1000000 }"
}

ms_create_cold=$(ns_to_ms "${ns_create_cold}")
ms_create_pool=$(ns_to_ms "${ns_create_pool}")
ms_shell_persistent=$(ns_to_ms "${ns_shell_persistent}")
ms_shell_oneshot=$(ns_to_ms "${ns_shell_oneshot}")
ms_file=$(ns_to_ms "${ns_file}")
ms_code_warm=$(ns_to_ms "${ns_code_warm}")
ms_first_code=$(ns_to_ms "${ns_first_code}")
ms_first_prewarm=$(ns_to_ms "${ns_first_prewarm}")
ms_e2e=$(ns_to_ms "${ns_e2e}")

# ── Compute speedup vs Docker baseline ────────────────────────────────────────
speedup() {
    local measured="$1"   # ms (decimal)
    local baseline="$2"   # ms (integer)
    if [[ "${measured}" == "N/A" ]]; then
        printf "N/A"
        return
    fi
    awk "BEGIN { printf \"%.1fx\", ${baseline}/${measured} }"
}

sp_create_cold=$(speedup "${ms_create_cold}" "${DOCKER_CREATE}")
sp_create_pool=$(speedup "${ms_create_pool}" "${DOCKER_CREATE}")
sp_shell_persistent=$(speedup "${ms_shell_persistent}" "${DOCKER_SHELL}")
sp_shell_oneshot=$(speedup "${ms_shell_oneshot}" "${DOCKER_SHELL}")
sp_file=$(speedup "${ms_file}" "${DOCKER_FILE}")
sp_code_warm=$(speedup "${ms_code_warm}" "${DOCKER_CODE_WARM}")
sp_first_code=$(speedup "${ms_first_code}" "${DOCKER_FIRST_CODE}")
sp_first_prewarm=$(speedup "${ms_first_prewarm}" "${DOCKER_FIRST_CODE}")
sp_e2e=$(speedup "${ms_e2e}" "${DOCKER_E2E}")

# ── Colour-code measured value against target ─────────────────────────────────
colour_ms() {
    local measured="$1"
    local target="$2"
    if [[ "${measured}" == "N/A" ]]; then
        printf "${YLW}%-10s${RST}" "${measured}"
        return
    fi
    local ok
    ok=$(awk "BEGIN { print (${measured} <= ${target}) ? \"yes\" : \"no\" }")
    if [[ "${ok}" == "yes" ]]; then
        printf "${GRN}%-10s${RST}" "${measured} ms"
    else
        printf "${RED}%-10s${RST}" "${measured} ms"
    fi
}

# ── Print table ───────────────────────────────────────────────────────────────
SEP="═══════════════════════════════════════════════════════════════════"
DIV="───────────────────────────────────────────────────────────────────"

printf "\n"
printf "  ix libkrun Benchmark Results\n"
printf "  %s\n" "${SEP}"
printf "  %-24s %-10s %-10s %-18s %s\n" \
    "Operation" "Measured" "Target" "Docker Baseline" "Speedup"
printf "  %s\n" "${DIV}"

row() {
    local label="$1" measured="$2" target="$3" baseline="$4" speedup="$5"
    printf "  %-24s " "${label}"
    colour_ms "${measured}" "${target}"
    printf " %-10s %-18s %s\n" "<${target}ms" "${baseline}ms" "${speedup}"
}

row "Creation (cold)"       "${ms_create_cold}"       "${TARGET_CREATE_COLD}"        "${DOCKER_CREATE}"     "${sp_create_cold}"
row "Creation (pool)"       "${ms_create_pool}"        "${TARGET_CREATE_POOL}"        "${DOCKER_CREATE}"     "${sp_create_pool}"
row "Shell (persistent)"    "${ms_shell_persistent}"   "${TARGET_SHELL_PERSISTENT}"   "${DOCKER_SHELL}"      "${sp_shell_persistent}"
row "Shell (one-shot)"      "${ms_shell_oneshot}"      "${TARGET_SHELL_ONESHOT}"      "${DOCKER_SHELL}"      "${sp_shell_oneshot}"
row "File R+W"              "${ms_file}"               "${TARGET_FILE}"               "${DOCKER_FILE}"       "${sp_file}"
row "Code exec (warm)"      "${ms_code_warm}"          "${TARGET_CODE_WARM}"          "${DOCKER_CODE_WARM}"  "${sp_code_warm}"
row "First code exec"       "${ms_first_code}"         "${TARGET_FIRST_CODE}"         "${DOCKER_FIRST_CODE}" "${sp_first_code}"
row "First code (pre-warm)" "${ms_first_prewarm}"      "${TARGET_FIRST_CODE_PREWARM}" "${DOCKER_FIRST_CODE}" "${sp_first_prewarm}"
row "E2E agent cycle"       "${ms_e2e}"                "${TARGET_E2E}"                "${DOCKER_E2E}"        "${sp_e2e}"

printf "  %s\n\n" "${SEP}"
printf "  ${GRN}green${RST} = at or below target   ${RED}red${RST} = above target\n\n"

exit "${bench_status}"
