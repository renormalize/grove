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

# run-scale-suite.sh - End-to-end scale test orchestration.
#
# Brings up the KWOK scale cluster, starts the all-component usage profiler in the
# background, runs the scale test in the foreground, then stops the profiler (printing
# its per-namespace summary) and optionally tears the cluster down. The profiler and the
# test run in parallel so CPU/memory samples cover the whole run.
#
# Usage:
#   hack/run-scale-suite.sh [SCALE]
#
# SCALE is one of: 1x (default), 10x, 100x. It selects the cluster preset and the
# matching PCS replica count:
#
#   1x    ->  100 nodes,    500 replicas  (1,000 pods)
#   10x   -> 1000 nodes,   5000 replicas  (10,000 pods)
#   100x  -> 10000 nodes, 50000 replicas  (100,000 pods)
#
# Overrides (env vars):
#   REPLICAS=<n>       PCS replicas (pods = replicas * 2). Overrides the SCALE default.
#   NODES=<n>          KWOK node count. Overrides the SCALE preset (uses scale.yaml + --set).
#   TEST_PATTERN=<re>  go test -run pattern (default: Test_ScaleTest).
#   DIAG_DIR=<path>    Output dir for CSVs + pprof (default: ./diag/scale-<scale>-<ts>).
#   PROFILE_INTERVAL=<s>  Profiler sample interval seconds (default: 5).
#   KEEP_CLUSTER=1     Do NOT tear the cluster down at the end (default: tear down).
#   SKIP_TEARDOWN=1    Alias for KEEP_CLUSTER=1.
#
# Examples:
#   hack/run-scale-suite.sh 10x
#   REPLICAS=20000 hack/run-scale-suite.sh 100x
#   NODES=2000 REPLICAS=8000 hack/run-scale-suite.sh
#   KEEP_CLUSTER=1 hack/run-scale-suite.sh 1x

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
if [[ ! -x "${UV}" ]]; then
  echo "ERROR: uv not found on PATH or at ${UV}. Install uv, or run 'make -C ${OPERATOR_DIR} scale-cluster-up' once to bootstrap it into hack/tools/bin." >&2
  exit 1
fi

SCALE="${1:-1x}"

# Map SCALE -> make target for cluster bring-up + default replica count.
case "${SCALE}" in
  1x)   CLUSTER_TARGET="scale-cluster-up"      DEFAULT_REPLICAS=500   ;;
  10x)  CLUSTER_TARGET="scale-cluster-up-10x"  DEFAULT_REPLICAS=5000  ;;
  100x) CLUSTER_TARGET="scale-cluster-up-100x" DEFAULT_REPLICAS=50000 ;;
  *)
    echo "ERROR: unknown SCALE '${SCALE}' (expected 1x, 10x, or 100x)" >&2
    exit 1
    ;;
esac

REPLICAS="${REPLICAS:-${DEFAULT_REPLICAS}}"
TEST_PATTERN="${TEST_PATTERN:-Test_ScaleTest}"
PROFILE_INTERVAL="${PROFILE_INTERVAL:-5}"
TIMESTAMP="$(date +%Y%m%d-%H%M%S)"
DIAG_DIR="${DIAG_DIR:-${OPERATOR_DIR}/diag/scale-${SCALE}-${TIMESTAMP}}"

# NODES override forces the generic scale.yaml preset with an explicit node count,
# bypassing the SCALE-specific preset.
CREATE_FLAGS=""
if [[ -n "${NODES:-}" ]]; then
  CLUSTER_TARGET="scale-cluster-up"
  CREATE_FLAGS="--set kwok.nodes=${NODES}"
fi

if [[ -n "${SKIP_TEARDOWN:-}" ]]; then
  KEEP_CLUSTER=1
fi

PODS=$(( REPLICAS * 2 ))
PROFILER_PID=""

log() { echo "==> $*"; }

cleanup() {
  local status=$?
  # Stop the profiler first so its SIGINT handler prints the summary and flushes CSVs.
  if [[ -n "${PROFILER_PID}" ]] && kill -0 "${PROFILER_PID}" 2>/dev/null; then
    log "Stopping profiler (pid ${PROFILER_PID})..."
    kill -INT "${PROFILER_PID}" 2>/dev/null || true
    wait "${PROFILER_PID}" 2>/dev/null || true
  fi
  if [[ -z "${KEEP_CLUSTER:-}" ]]; then
    log "Tearing down scale cluster..."
    make -C "${OPERATOR_DIR}" scale-cluster-down || true
  else
    log "KEEP_CLUSTER set - leaving cluster running. Tear down later with: make -C ${OPERATOR_DIR} scale-cluster-down"
  fi
  exit "${status}"
}
trap cleanup EXIT INT TERM

mkdir -p "${DIAG_DIR}"

log "Scale suite: ${SCALE}"
log "  cluster target : ${CLUSTER_TARGET}${CREATE_FLAGS:+ (${CREATE_FLAGS})}"
log "  replicas       : ${REPLICAS}  (=> ${PODS} pods)"
log "  test pattern   : ${TEST_PATTERN}"
log "  diag dir       : ${DIAG_DIR}"

# 1. Bring up the cluster.
log "Bringing up cluster..."
make -C "${OPERATOR_DIR}" "${CLUSTER_TARGET}" E2E_CREATE_FLAGS="${CREATE_FLAGS}"

# 2. Start the profiler in the background (deploys metrics-server, then samples every N s).
#    It writes usage-pods.csv + usage-server.csv under DIAG_DIR and prints a summary on SIGINT.
log "Starting profiler (interval ${PROFILE_INTERVAL}s)..."
"${SCRIPT_DIR}/deploy-addons.sh" --metrics-server
( cd "${OPERATOR_DIR}" && "${UV}" run "${SCRIPT_DIR}/profile-usage.py" \
    --out "${DIAG_DIR}" --interval "${PROFILE_INTERVAL}" ) \
  >"${DIAG_DIR}/profiler.log" 2>&1 &
PROFILER_PID=$!
log "Profiler running (pid ${PROFILER_PID}), logging to ${DIAG_DIR}/profiler.log"

# 3. Run the scale test in the foreground (profiler keeps sampling in parallel).
log "Running scale test..."
SCALE_PCS_REPLICAS="${REPLICAS}" \
  make -C "${OPERATOR_DIR}" run-scale-test \
    TEST_PATTERN="${TEST_PATTERN}" \
    DIAG_DIR="${DIAG_DIR}"

# 4. cleanup() (via trap) stops the profiler and tears down the cluster.
log "Scale test finished. Artifacts in ${DIAG_DIR}"
