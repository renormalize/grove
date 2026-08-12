# Scale Test pprof Analysis — Grove Operator

Analysis of the pprof/diagnostic artifacts under `operator/diag/` from two scale-test
suites. Source phase timings come from `scale-test-results.json`; resource peaks from
`usage-*.csv` and `profiler.log`; hotspots from `go tool pprof` on the `*.pprof.gz` files.

## What was run

Two full scale-test suites, same 7 phases each, different sizes. The scale factor derives
from `SCALE_PCS_REPLICAS` (see `e2e/tests/scale/scale_test.go`: `500 -> 1x`, `5000 -> 10x`).
Each PCS replica materializes 2 pods.

| Run | PCS replicas | Pods | Operator image |
|---|---|---|---|
| **1x** (`scale-1x-20260804-130619`) | 500 | ~1,000 | `…be75c7ed` |
| **10x** (`scale-10x-20260805-062946`) | 5,000 | ~10,000 | `…d6316ad` |

Client config both runs: QPS=100, Burst=150. Controller concurrency = 20 each for
PodCliqueSet / PodCliqueScalingGroup / PodClique.

## Headline timings (`scale-test-results.json`)

| Phase | 1x | 10x | Scaling |
|---|---|---|---|
| ScaleTest deploy → pods-ready | 54s | **917s** | ~17x (super-linear) |
| ScaleTest delete | 0.1s | 33s | — |
| ScaleDown_ToZero deploy (500 → ready) | 53s | **548s** | ~10x |
| ScaleDown_ToZero scale-down | 57s | **830s** | ~14x |
| ScaleDown (½x) scale-down | 32s | 460s | ~14x |

Deploy and scale-down both scale **worse than linearly** at 10x, pointing to a super-linear
algorithm. The profiles confirm it.

## Resource peaks (`usage-*.csv` / `profiler.log`)

| Metric | 1x | 10x |
|---|---|---|
| grove-operator mem (max) | 283 MB | **1,735 MB** (~6.5x) |
| grove-operator CPU (max) | 690m | **1,563m** |
| k3d server (etcd + apiserver) mem | — | **15 GB** |
| k3d server CPU | — | ~2,580m sustained |

## Finding 1 — O(N²) FQN rebuild dominates deploy CPU (application bug)

In the 10x deploy CPU profile, **9.75% of all CPU (35.76s of 367s)** is a single line:

`internal/controller/podclique/reconcilespec.go:113`
```go
if !slices.Contains(componentutils.GetPodCliqueFQNsForPCSNotInPCSG(pcs), pclq.Name) {
```

`GetPodCliqueFQNsForPCSNotInPCSG` (`internal/controller/common/component/utils/podcliqueset.go:36`)
loops over **all** `pcs.Spec.Replicas` and builds each name via `fmt.Sprintf`
(`GeneratePodCliqueName`, `api/common/namegen.go:79`). It is called **once per PodClique
reconcile**, and there are N PodCliques — so the total work is **O(N²) Sprintf calls**.

This is why the top of the CPU profile is `fmt.Sprintf` (8.5% cum) → `GeneratePodCliqueName`
(7.9% cum) and why deploy scales ~17x for a 10x workload. The same rebuild-and-linear-scan
anti-pattern also appears at `internal/controller/podclique/register.go:201`.

**Fix direction:** build the FQN set once per reconcile (or once per PCS generation) and do
membership via a set/map lookup instead of rebuilding the full slice + linear
`slices.Contains` on every PodClique.

## Finding 2 — ~10k goroutines parked on the client rate limiter (throughput ceiling)

The 10x deploy goroutine profile shows **11,645 goroutines**, of which **10,208 (87.5%)** are
blocked in:

```
service._resource.Sync.func1 → doCreateOrUpdate → rate.Limiter.WaitN → gopark
```

`service.Sync` (`internal/controller/podcliqueset/components/service/service.go:76`) creates
one headless Service **per PCS replica** (5,000 at 10x) and fans them **all out concurrently**
via `utils.RunConcurrently`. Every task then serializes on the shared client QPS limiter
(100 QPS / 150 burst). The operator spawns thousands of goroutines that just queue behind the
limiter — deploy time is gated by `objects ÷ QPS`, not by CPU.

This is **not a leak** (goroutine count falls 11,645 → 9,708 → 7,750 across
deploy/steady/delete, tracking load). But it is a scalability bottleneck and a memory cost
(each parked goroutine holds a stack + the closure's captured PCS).

**Fix direction:** bound the fan-out (worker pool instead of goroutine-per-object) and/or
raise QPS/Burst for large deployments. Unbounded fan-out mostly wastes memory since the
limiter serializes the work anyway.

## Finding 3 — memory dominated by informer cache + patch churn (mostly expected)

10x operator inuse_space ~1.2 GB (heap), RSS peak 1.7 GB. Top allocations:

- **Informer cache** unmarshaling Pods: `FieldsV1.Unmarshal` (11.7%), `ObjectMeta.Unmarshal`
  (21% cum), `PodSpec`/`Container.Unmarshal`. Expected for caching 10k pods — scales linearly.
- **Patch path:** `evanphx/json-patch` + `reflect.deepValueEqual` +
  `unstructuredConverter.ToUnstructured` (~7% CPU cum on deploy). Every reconcile does
  `CreateOrPatch`, which round-trips objects through unstructured + a full deep-equal. This is
  per-object overhead multiplied by the O(N²) reconcile pressure from Finding 1 — fixing
  Finding 1 reduces this too.

`managedFields` (`FieldsV1`) is a notable chunk of cache memory; if the operator does not need
it, stripping managedFields via a cache transform would cut heap meaningfully.

## GC / runtime note

CPU profiles across every phase are ~40–50% GC/runtime (`mallocgc`, `scanSpan`,
`tryDeferToSpanScan`, `spanClass.sizeclass`). That is a **symptom** of the allocation rate from
Findings 1 and 3, not a separate problem — cutting the Sprintf storm and patch churn is what
brings GC down.

## Recommended priority

1. **Fix the O(N²) FQN membership check** (Finding 1) — biggest, cheapest win; directly
   attacks the super-linear deploy time.
2. **Bound the service Sync fan-out / tune QPS** (Finding 2) — removes the 10k-goroutine
   throughput ceiling.
3. **Strip managedFields from the cache** (Finding 3) — cuts steady-state heap.

## Not yet analyzed

- Scale-down profiles line-by-line (they scale ~14x; only deploy/steady-state were broken
  down in detail).
- ScaleUp / ScaleUp_FromZero phase profiles in the 10x run.
