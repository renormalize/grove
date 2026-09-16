# Running the Grove operator scale tests

A practical guide to running the scale-test suite that measures operator performance
across scale, workload shape, and PodCliqueSet spread. It covers the one-shot matrix
sweep (the normal path) and the manual per-run path (for debugging a single point).

Everything below runs from the `operator/` directory.

---

## 1. What the suite measures

Each **suite run** exercises three tests against a freshly deployed workload:

- `Test_ScaleTest`  — deploy → steady-state no-op reconcile → delete latency.
- `Test_ScaleUp*`   — deploy at half size, then grow to target (measured phase).
- `Test_ScaleDown*` — deploy at full size, then shrink (measured phase).

Three independent knobs shape a run:

| Axis | Knob | Values | Effect |
|---|---|---|---|
| **Scale** | `SCALE` / `SCALE_PCS_REPLICAS` | `1x` / `10x` / `50x` / `100x` | 1k / 10k / 50k / 100k total pods |
| **Shape** | `SCALE_WORKLOAD` | `flat` / `disagg` | standalone cliques vs. disaggregated prefill+decode LLM topology |
| **Spread** | `SCALE_PCS_COUNT` | `1` / `N` | one wide PodCliqueSet vs. N smaller ones (same total pods) |

The total pod count is held constant across shape and spread at a given scale, so all
points are directly comparable. `disagg` and multi-PCS scale-up/down measure *different*
operator code paths (growing PCSG replicas / adding whole PCS objects), not just size.

Each run collects two kinds of profiling data into its diag dir:

- **pprof** — CPU + memory + goroutine profiles of `grove-operator`, captured **once per
  test phase** (via Pyroscope). Files: `pprof-<runID>-<phase>-<type>.pprof.gz`.
- **Resource-usage CSVs** — `usage-pods.csv` (per-pod CPU/mem across grove/kai/kube-system/
  pyroscope namespaces) and `usage-server.csv` (the k3d server container = k3s control
  plane: apiserver + KCM + scheduler + etcd), sampled on an interval, plus `profiler.log`.

---

## 2. Prerequisites (one-time)

- Docker running (k3d + KWOK virtual nodes + `docker stats` for the control-plane sample).
- `uv` — bootstrapped automatically into `hack/tools/bin` the first time you run any
  `scale-cluster-up` target; nothing to install by hand.
- Enough host headroom. 100x (10,000 KWOK nodes + 100k pods on a single k3d server with
  embedded etcd) is at the edge of what this backend sustains — expect high etcd
  memory/disk.

---

## 3. The normal path: matrix sweep

`hack/run-scale-suite.sh` sweeps the **full scale × spread matrix for one shape**, a fresh
cluster per combo, continue-on-failure, with a pass/fail/skip summary at the end.

**Parallelize by shape** — run one shape per machine:

```bash
# machine A
./hack/run-scale-suite.sh flat

# machine B
./hack/run-scale-suite.sh disagg
```

Each invocation runs, per default lists `SCALES="1x 10x 50x 100x"` and
`PCS_COUNTS="1 10 50 100"`:

- **flat** → 16 combos (all valid).
- **disagg** → 15 combos; `disagg / 1x / count=100` is auto-skipped (too few replicas to
  spread across 100 PCS without a zero-replica object) and reported as `SKIP`.

### Preview before committing hours of runtime

`DRY_RUN=1` prints every resolved combo (cluster target, replicas, timeout, profile
interval, diag dir) and the exact `make` command, touching no Docker:

```bash
DRY_RUN=1 ./hack/run-scale-suite.sh flat
DRY_RUN=1 ./hack/run-scale-suite.sh disagg   # confirms the one SKIP
```

### Narrowing the sweep

Override the swept lists to run a subset — useful for a quick smoke or re-running one point:

```bash
SCALES=1x PCS_COUNTS=1 ./hack/run-scale-suite.sh flat        # single smallest combo
SCALES="1x 10x 50x" ./hack/run-scale-suite.sh disagg         # skip 100x
PCS_COUNTS="1 10" ./hack/run-scale-suite.sh flat             # wide + one multi-PCS point
```

### Key env overrides (apply to every combo)

| Env | Default | Purpose |
|---|---|---|
| `PROFILE_INTERVAL` | per-scale: 1x=5s, 10x=30s, 50x=60s, 100x=60s | usage-CSV sample interval. Longer tiers sample less often → smaller CSVs to fetch. Set to force one interval everywhere. |
| `GO_TEST_TIMEOUT` | per-scale: 45m / 180m / 600m | `go test -timeout`. |
| `TEST_PATTERN` | empty (all tests) | `go test -run` regex, e.g. `Test_ScaleTest`. |
| `REPLICAS` | per-scale default | override PCS replicas for every scale. |
| `NODES` | per-scale preset | force an explicit KWOK node count (uses `scale.yaml` + `--set`). |
| `DIAG_ROOT` | `<operator>/diag` | parent dir for per-combo diag dirs. |
| `KEEP_CLUSTER=1` | (tear down) | leave the last combo's cluster up for inspection. |

### Where results land

Per combo: `diag/scale-<shape>-<scale>-pcs<count>-<timestamp>/` containing the
`pprof-*.pprof.gz` files, `usage-pods.csv`, `usage-server.csv`, and `profiler.log`.

---

## 4. The manual path: a single run against your own cluster

Use this to debug one point or iterate without the sweep's per-combo teardown.

```bash
# 1. Bring up a cluster at the scale you want (pick one target):
make scale-cluster-up            # 1x  — 100 KWOK nodes
make scale-cluster-up-10x        # 10x — 1000 nodes
make scale-cluster-up-50x        # 50x — 5000 nodes
make scale-cluster-up-100x       # 100x — 10000 nodes
#    Arbitrary size (≤ ~65,000 nodes, the 10.0.x.x IP-space cap):
#    make scale-cluster-up E2E_CREATE_FLAGS="--set kwok.nodes=30000"

# 2. (Optional) run the usage profiler in a SECOND terminal for the whole run:
make profile-usage DIAG_DIR=./diag/manual PROFILE_INTERVAL=5
#    Ctrl-C to stop and print the per-namespace max/mean summary.

# 3. Run the tests. Set the knobs via env:
SCALE_PCS_REPLICAS=5000 SCALE_WORKLOAD=disagg SCALE_PCS_COUNT=10 \
  make run-scale-test TEST_PATTERN=Test_ScaleTest GO_TEST_TIMEOUT=180m DIAG_DIR=./diag/manual

# 4. Tear down when done:
make scale-cluster-down
```

`SCALE_PCS_REPLICAS` drives the scale multiplier: 500→1x, 5000→10x, 25000→50x, 50000→100x.

---

## 5. Fetching results to your Mac

pprof files are gzipped and per-phase (fixed count, not time-based), and the usage CSVs
sample coarser at higher tiers — so a full sweep is a few hundred small files. Tar the
diag tree and pull it in one shot:

```bash
# on the test host
tar czf scale-diag.tgz -C operator diag/
# from your Mac
scp <host>:/path/to/operator/scale-diag.tgz .
```

Open a CPU profile locally with:

```bash
go tool pprof -http=: pprof-<runID>-<phase>-cpu.pprof.gz
```

---

## 6. Quick reference

```bash
# full flat matrix (machine A)
./hack/run-scale-suite.sh flat
# full disagg matrix (machine B)
./hack/run-scale-suite.sh disagg
# preview any sweep without clusters
DRY_RUN=1 ./hack/run-scale-suite.sh <flat|disagg>
# one fast combo
SCALES=1x PCS_COUNTS=1 ./hack/run-scale-suite.sh flat
# tear down a leftover cluster
make scale-cluster-down
```
