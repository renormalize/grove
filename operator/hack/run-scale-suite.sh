#!/usr/bin/env bash
# Copyright 2026 The Grove Authors.
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

set -o errexit
set -o nounset
set -o pipefail

# run-scale-suite.sh - End-to-end scale test matrix sweep for a single workload shape.
#
# Sweeps the full scale x PCS-spread matrix for ONE workload shape (flat or disagg),
# running every scale test at every point. Each matrix point ("combo") gets a fresh
# cluster: bring up -> start the all-component usage profiler in the background -> run the
# scale test in the foreground -> stop the profiler (printing its per-namespace summary) ->
# tear the cluster down. A failing combo does not stop the sweep; a pass/fail/skip summary
# is printed at the end.
#
# Parallelize by shape: run `run-scale-suite.sh flat` on one machine and
# `run-scale-suite.sh disagg` on another.
#
# Usage:
#   hack/run-scale-suite.sh [SHAPE]
#
# SHAPE is one of: flat (default) or disagg.
#   flat   = one standalone PodClique per PCS replica (pods = replicas * 2).
#   disagg = disaggregated LLM shape (prefill+decode PCSGs, decode-heavy, tensor-parallel
#            groups) sized to the same total pod count.
#
# The swept axes (fixed lists, overridable via env for narrower sweeps):
#   SCALES="1x 10x 50x 100x"  Scale tiers. Each selects a cluster preset + replica count:
#                             1x   ->   100 nodes,    500 replicas  (1,000 pods)
#                             10x  ->  1000 nodes,   5000 replicas  (10,000 pods)
#                             50x  ->  5000 nodes,  25000 replicas  (50,000 pods)
#                             100x -> 10000 nodes,  50000 replicas  (100,000 pods)
#   PCS_COUNTS="1 10 50 100"  PCS-spread points: how many separate PodCliqueSet objects the
#                             same total pod count is split across. 1 = one wide PCS; N>1 =
#                             N smaller PCS (stresses per-PCS operator overhead). The total
#                             pod count is identical across all N, so points are comparable.
#
# Combos that would render a zero-replica PodCliqueSet (too many PCS for the available
# replicas, e.g. disagg/1x with PCS_COUNT=100) are auto-skipped and reported as SKIP.
#
# Other overrides (env vars, applied to every combo):
#   NODES=<n>          KWOK node count. Overrides the scale preset (uses scale.yaml + --set).
#   REPLICAS=<n>       PCS replicas. Overrides the per-scale default (applied to every scale).
#   TEST_PATTERN=<re>  go test -run pattern (default: empty = run all scale tests).
#   PROFILE_INTERVAL=<s>  Profiler sample interval seconds. Default scales with each tier
#                         (1x->5s, 10x->30s, 50x->60s, 100x->60s) so long runs produce
#                         smaller usage CSVs; set this to force one interval across every combo.
#   GO_TEST_TIMEOUT=<d>   go test -timeout value (default scales with each scale tier:
#                         1x->45m, 10x->180m, 50x->600m, 100x->600m). Go duration string.
#   DIAG_ROOT=<path>   Parent dir for per-combo diag dirs (default: <operator>/diag).
#   KEEP_CLUSTER=1     Do NOT tear clusters down (leaves only the LAST combo's cluster up).
#   SKIP_TEARDOWN=1    Alias for KEEP_CLUSTER=1.
#   DRY_RUN=1          Print each resolved combo + the exact `make` command and skip all
#                      Docker/cluster work. Use to validate the matrix and skip logic.
#
# Examples:
#   hack/run-scale-suite.sh flat            # full 4x4 matrix, flat shape
#   hack/run-scale-suite.sh disagg          # full matrix, disagg shape (disagg/1x/100 skipped)
#   DRY_RUN=1 hack/run-scale-suite.sh flat  # preview the matrix without touching Docker
#   SCALES=1x PCS_COUNTS=1 hack/run-scale-suite.sh flat   # single smallest combo
#   SCALES="1x 10x 50x" hack/run-scale-suite.sh flat      # skip the 100x tier

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OPERATOR_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Resolve uv the same way the Makefile does (tools.mk): prefer a uv on PATH, else the
# copy the Makefile downloads into hack/tools/bin. We invoke the profiler via `uv run`
# explicitly rather than relying on its `#!/usr/bin/env -S uv run` shebang, so the script
# works even when uv is not on PATH (e.g. under a login shell or make-wrapped context).
UV="$(command -v uv 2>/dev/null || true)"
if [[ -z "${UV}" ]]; then
  UV="${OPERATOR_DIR}/../hack/tools/bin/uv"
fi
if [[ ! -x "${UV}" && -z "${DRY_RUN:-}" ]]; then
  echo "ERROR: uv not found on PATH or at ${UV}. Install uv, or run 'make -C ${OPERATOR_DIR} scale-cluster-up' once to bootstrap it into hack/tools/bin." >&2
  exit 1
fi

SHAPE="${1:-flat}"
case "${SHAPE}" in
  flat|disagg) ;;
  *)
    echo "ERROR: unknown SHAPE '${SHAPE}' (expected 'flat' or 'disagg')" >&2
    exit 1
    ;;
esac

# Swept axes. Space-separated so they can be overridden from the env for narrower sweeps.
SCALES="${SCALES:-1x 10x 50x 100x}"
PCS_COUNTS="${PCS_COUNTS:-1 10 50 100}"

# Shared per-combo overrides.
TEST_PATTERN="${TEST_PATTERN:-}"
# Empty means "use the per-scale default" (see scale_preset); set it to force one interval
# across every combo.
PROFILE_INTERVAL="${PROFILE_INTERVAL:-}"
DIAG_ROOT="${DIAG_ROOT:-${OPERATOR_DIR}/diag}"

if [[ -n "${SKIP_TEARDOWN:-}" ]]; then
  KEEP_CLUSTER=1
fi

# scale_preset SCALE -> "<cluster-target> <default-replicas> <default-timeout> <profile-interval>".
# The whole suite runs sequentially in one `go test` invocation, so the timeout must cover
# every scale test; cushioned for the slow single-etcd k3s control plane. The profile
# interval grows with the tier (longer runs sample less often, keeping the usage CSVs a
# manageable size to fetch); an explicit PROFILE_INTERVAL env var overrides it.
scale_preset() {
  case "$1" in
    1x)   echo "scale-cluster-up 500 45m 5" ;;
    10x)  echo "scale-cluster-up-10x 5000 180m 30" ;;
    50x)  echo "scale-cluster-up-50x 25000 600m 60" ;;
    100x) echo "scale-cluster-up-100x 50000 600m 60" ;;
    *)    return 1 ;;
  esac
}

# pods_per_pcs_replica SCALE -> pods contributed by one disagg PCS replica for that tier.
# Mirrors disaggTiers/podsPerPCSReplica in e2e/tests/scale/scale_test.go
# (1x:20, 10x:100, 50x:200, 100x:200). Flat has no PCS-replica notion; it uses 2 pods/replica.
pods_per_pcs_replica() {
  case "$1" in
    1x)   echo 20 ;;
    10x)  echo 100 ;;
    50x)  echo 200 ;;
    100x) echo 200 ;;
  esac
}

# replica_units SHAPE SCALE REPLICAS -> the count of divisible units the pods split into.
# flat splits by replica (2 pods each); disagg splits by PCS replica (podsPerPCSReplica
# each). This is the pool splitAcross() in scale_test.go divides among PCS objects.
replica_units() {
  local shape="$1" scale="$2" replicas="$3"
  local pods=$(( replicas * 2 ))
  if [[ "${shape}" == "disagg" ]]; then
    echo $(( pods / $(pods_per_pcs_replica "${scale}") ))
  else
    echo "${replicas}"
  fi
}

log() { echo "==> $*"; }

PROFILER_PID=""
CURRENT_CLUSTER_UP=""  # non-empty while a combo owns a live cluster the trap must reap.

# teardown_combo stops the running profiler (SIGINT so it flushes its CSV summary) and tears
# down the current combo's cluster unless KEEP_CLUSTER is set. Safe to call repeatedly.
teardown_combo() {
  if [[ -n "${PROFILER_PID}" ]] && kill -0 "${PROFILER_PID}" 2>/dev/null; then
    log "Stopping profiler (pid ${PROFILER_PID})..."
    kill -INT "${PROFILER_PID}" 2>/dev/null || true
    wait "${PROFILER_PID}" 2>/dev/null || true
  fi
  PROFILER_PID=""
  if [[ -n "${CURRENT_CLUSTER_UP}" ]]; then
    if [[ -z "${KEEP_CLUSTER:-}" ]]; then
      log "Tearing down scale cluster..."
      make -C "${OPERATOR_DIR}" scale-cluster-down || true
    else
      log "KEEP_CLUSTER set - leaving cluster running. Tear down later with: make -C ${OPERATOR_DIR} scale-cluster-down"
    fi
  fi
  CURRENT_CLUSTER_UP=""
}

# on_interrupt reaps whatever combo is in flight if the sweep is interrupted, so a Ctrl-C
# mid-matrix never leaks a running cluster. errexit-triggered EXIT also lands here.
on_interrupt() {
  local status=$?
  teardown_combo
  exit "${status}"
}
trap on_interrupt EXIT INT TERM

# run_one SHAPE SCALE PCS_COUNT -> runs one matrix combo end to end. Returns the scale
# test's exit status (0 = pass); never aborts the sweep on its own.
run_one() {
  local shape="$1" scale="$2" pcs_count="$3"

  local preset cluster_target default_replicas default_timeout default_interval
  preset="$(scale_preset "${scale}")"
  read -r cluster_target default_replicas default_timeout default_interval <<<"${preset}"

  local replicas go_timeout profile_interval create_flags=""
  replicas="${REPLICAS:-${default_replicas}}"
  go_timeout="${GO_TEST_TIMEOUT:-${default_timeout}}"
  profile_interval="${PROFILE_INTERVAL:-${default_interval}}"
  # NODES override forces the generic scale.yaml preset with an explicit node count.
  if [[ -n "${NODES:-}" ]]; then
    cluster_target="scale-cluster-up"
    create_flags="--set kwok.nodes=${NODES}"
  fi

  local pods=$(( replicas * 2 ))
  local ts diag_dir
  ts="$(date +%Y%m%d-%H%M%S)"
  diag_dir="${DIAG_ROOT}/scale-${shape}-${scale}-pcs${pcs_count}-${ts}"

  log "Combo: shape=${shape} scale=${scale} pcs_count=${pcs_count}"
  log "  cluster target : ${cluster_target}${create_flags:+ (${create_flags})}"
  log "  replicas       : ${replicas}  (=> ${pods} pods across ${pcs_count} PCS)"
  log "  go test timeout: ${go_timeout}"
  log "  profile interval: ${profile_interval}s"
  log "  diag dir       : ${diag_dir}"

  if [[ -n "${DRY_RUN:-}" ]]; then
    log "  [dry-run] make -C ${OPERATOR_DIR} ${cluster_target} E2E_CREATE_FLAGS=\"${create_flags}\""
    log "  [dry-run] SCALE_PCS_REPLICAS=${replicas} SCALE_WORKLOAD=${shape} SCALE_PCS_COUNT=${pcs_count} \\"
    log "            make -C ${OPERATOR_DIR} run-scale-test TEST_PATTERN=\"${TEST_PATTERN}\" GO_TEST_TIMEOUT=${go_timeout} DIAG_DIR=${diag_dir}"
    return 0
  fi

  mkdir -p "${diag_dir}"

  # 1. Bring up the cluster.
  log "Bringing up cluster..."
  CURRENT_CLUSTER_UP=1
  make -C "${OPERATOR_DIR}" "${cluster_target}" E2E_CREATE_FLAGS="${create_flags}"

  # 2. Start the profiler in the background (deploys metrics-server, then samples every N s).
  #    It writes usage-pods.csv + usage-server.csv under diag_dir and prints a summary on SIGINT.
  log "Starting profiler (interval ${profile_interval}s)..."
  "${SCRIPT_DIR}/deploy-addons.sh" --metrics-server
  ( cd "${OPERATOR_DIR}" && "${UV}" run "${SCRIPT_DIR}/profile-usage.py" \
      --out "${diag_dir}" --interval "${profile_interval}" ) \
    >"${diag_dir}/profiler.log" 2>&1 &
  PROFILER_PID=$!
  log "Profiler running (pid ${PROFILER_PID}), logging to ${diag_dir}/profiler.log"

  # 3. Run the scale test in the foreground (profiler keeps sampling in parallel). Capture
  #    the exit status without tripping errexit so a failing combo is recorded, not fatal.
  log "Running scale test..."
  local rc=0
  SCALE_PCS_REPLICAS="${replicas}" \
  SCALE_WORKLOAD="${shape}" \
  SCALE_PCS_COUNT="${pcs_count}" \
    make -C "${OPERATOR_DIR}" run-scale-test \
      TEST_PATTERN="${TEST_PATTERN}" \
      GO_TEST_TIMEOUT="${go_timeout}" \
      DIAG_DIR="${diag_dir}" || rc=$?

  # 4. Stop the profiler and tear this combo's cluster down before the next combo.
  teardown_combo
  log "Combo finished (rc=${rc}). Artifacts in ${diag_dir}"
  return "${rc}"
}

# --- Matrix sweep -----------------------------------------------------------------------

log "Scale suite matrix: shape=${SHAPE}"
log "  scales     : ${SCALES}"
log "  pcs counts : ${PCS_COUNTS}"
log "  test pattern: ${TEST_PATTERN:-<all>}"
[[ -n "${DRY_RUN:-}" ]] && log "  DRY_RUN     : no clusters will be created"

RESULTS=()
FAILED=0

for scale in ${SCALES}; do
  if ! scale_preset "${scale}" >/dev/null; then
    log "SKIP shape=${SHAPE} scale=${scale} (unknown scale tier)"
    RESULTS+=("${scale} <all>: SKIP (unknown scale)")
    continue
  fi
  read -r _ default_replicas _ <<<"$(scale_preset "${scale}")"
  replicas="${REPLICAS:-${default_replicas}}"
  units="$(replica_units "${SHAPE}" "${scale}" "${replicas}")"

  for pcs_count in ${PCS_COUNTS}; do
    # Auto-skip combos where the replicas can't spread across pcs_count PCS objects without
    # some object getting zero replicas (matches splitAcross() in scale_test.go). Integer
    # division: skip when units/pcs_count == 0.
    if (( units / pcs_count < 1 )); then
      log "SKIP shape=${SHAPE} scale=${scale} count=${pcs_count} (would render zero-replica PCS: ${units} units / ${pcs_count})"
      RESULTS+=("${scale} pcs=${pcs_count}: SKIP (zero-replica PCS)")
      continue
    fi

    if run_one "${SHAPE}" "${scale}" "${pcs_count}"; then
      RESULTS+=("${scale} pcs=${pcs_count}: PASS")
    else
      RESULTS+=("${scale} pcs=${pcs_count}: FAIL")
      FAILED=1
    fi
  done
done

# --- Summary ----------------------------------------------------------------------------

echo
log "==================== Matrix summary (shape=${SHAPE}) ===================="
for r in "${RESULTS[@]}"; do
  echo "    ${r}"
done
log "========================================================================"

# The trap runs on EXIT; nothing left to reap here since run_one tears down per combo.
exit "${FAILED}"
