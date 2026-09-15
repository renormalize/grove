# Grove Operator — Performance Hotspot Analysis (High Load)

**Purpose:** Identify all the relevant places to optimize the Grove operator under high load,
derived from the pprof/diagnostic artifacts under `operator/diag/`. This document reports
findings only — it does not propose code changes.

**Inputs analyzed:** `operator/diag/scale-10x-20260805-062946/` (5,000 PCS replicas, ~10,000
pods) and `operator/diag/scale-1x-20260804-130619/` (500 replicas, ~1,000 pods). Client config
both runs: QPS=100, Burst=150. Controller concurrency = 20. The 10x run is where the
super-linear behavior is visible, so it drives the findings below.

This extends the pre-existing `scale-analysis.md`: its numbers were confirmed against the raw
profiles, and this doc adds the steady-state patch-churn breakdown, the full inventory of
unbounded fan-out call sites, and the observation that the fix patterns already exist in the
codebase.

---

## How the analysis was performed (methods)

Toolchain: `go tool pprof` (go1.26.4). No local operator binary was needed — symbol names are
embedded in the profiles, so function-level attribution works without the binary. Source
line-level `-list` output resolves for in-repo files (module paths under
`internal/`); it does not resolve for the `api@v0.0.0` vendored module path (`namegen.go`), which
is why `GeneratePodCliqueName` shows a "could not find file" error under `-list` but still
attributes correctly under `-top`.

Profiles exist per phase (`operator-baseline`, `deploy`, `steady-state-reconcile` / `scale-up` /
`scale-down`, `delete`, `final-check`) and per type (`-cpu`, `-memory`, `-goroutine`). Commands
run, all from `operator/diag/`:

- **CPU, cumulative (identifies which call trees dominate):**
  `go tool pprof -top -cum -nodecount=40 <phase>-cpu.pprof.gz`
  Applied to: 10x deploy, 10x steady-state, 10x scale-down (ScaleDown_ToZero).

- **CPU, flat/self (identifies leaf hot spots — where cycles are actually spent):**
  `go tool pprof -top -nodecount=30 <phase>-cpu.pprof.gz`
  Applied to: 10x deploy. Confirmed the leaf cost is GC scan + `fmt.Sprintf` internals.

- **CPU, source line attribution (pins the cost to a specific line):**
  `go tool pprof -list '<func regex>' <phase>-cpu.pprof.gz`
  Applied to: `shouldCheckPendingUpdatesForPCLQ`, `GetPodCliqueFQNsForPCSReplicaNotInPCSG`,
  `GeneratePodCliqueName`.

- **Goroutine (identifies the concurrency/blocking ceiling — where goroutines are parked):**
  `go tool pprof -top -nodecount=25 <phase>-goroutine.pprof.gz`
  Applied to: 10x deploy, 10x delete. Read the parked stack via `-cum` ordering.

- **Memory `inuse_space`, cumulative (identifies what holds live heap):**
  `go tool pprof -top -cum -inuse_space -nodecount=30 <phase>-memory.pprof.gz`
  Applied to: 10x deploy.

Each profile's header (`Duration`, `Total samples`, sampling %) was recorded to keep percentages
honest — e.g. the 10x deploy CPU profile is `Duration 916.75s, Total samples 366.98s (40.03%)`,
so "9.75% of total" means 9.75% of 366.98 sampled CPU-seconds, i.e. ~35.8s.

After profiling, findings were traced to source with `grep`/Read across `internal/` and `api/` to
(a) confirm the algorithm, (b) enumerate all call sites of each hot function, and (c) check
whether a better pattern already exists in the tree.

---

## Headline timings (from `scale-test-results.json`)

| Phase | 1x | 10x | Scaling |
|---|---|---|---|
| ScaleTest deploy → pods-ready | 54s | 917s | ~17x (super-linear) |
| ScaleDown_ToZero deploy (500 → ready) | 53s | 548s | ~10x |
| ScaleDown_ToZero scale-down | 57s | 830s | ~14x |

Deploy scaling ~17x for a 10x workload → a super-linear algorithm. The profiles confirm it.

## Resource peaks (`usage-*.csv` / `profiler.log`)

| Metric | 1x | 10x |
|---|---|---|
| grove-operator mem (max) | 283 MB | 1,735 MB (~6.5x) |
| grove-operator CPU (max) | 690m | 1,563m |
| k3d server (etcd + apiserver) mem | — | ~15 GB |

---

## Finding 1 — O(N²) FQN rebuild dominates deploy CPU

**Profile evidence (10x deploy CPU, `-top -cum`):**
```
35.77s  9.75%  podclique.shouldCheckPendingUpdatesForPCLQ
35.58s  9.70%  common/component/utils.GetPodCliqueFQNsForPCSNotInPCSG
34.13s  9.30%  common/component/utils.GetPodCliqueFQNsForPCSReplicaNotInPCSG
31.11s  8.48%  fmt.Sprintf
29.16s  7.95%  api/common.GeneratePodCliqueName
```
Flat/self profile confirms the leaf cost is `runtime.spanClass.sizeclass` (9.13%),
`fmt.(*pp).doPrintf` (6.30% cum), `fmt.(*pp).printArg` (4.96%) — i.e. the `Sprintf` allocation
storm, plus the GC that allocation drives.

**`-list` attribution:**
- `shouldCheckPendingUpdatesForPCLQ` (`reconcilespec.go`): 35.76s of its 35.77s is the single
  line 113 — `if !slices.Contains(GetPodCliqueFQNsForPCSNotInPCSG(pcs), pclq.Name)`.
- `GetPodCliqueFQNsForPCSReplicaNotInPCSG` (`podcliqueset.go`): 29.40s of 34.13s is line 49 — the
  `append(..., GeneratePodCliqueName(...))` call.

**Root cause:** `GetPodCliqueFQNsForPCSNotInPCSG`
(`internal/controller/common/component/utils/podcliqueset.go:36-53`) loops over all
`pcs.Spec.Replicas`, `Sprintf`-building every PodClique name. It is rebuilt from scratch **on
every PodClique reconcile**, then scanned linearly with `slices.Contains` — so total work is
**O(N²) Sprintf calls**. This is why deploy scales ~17x for a 10x workload.

**All call sites of the O(N) builder (`grep`, non-test):**
| Location | Usage | Hot? |
|---|---|---|
| `internal/controller/podclique/reconcilespec.go:113` | rebuild + linear `slices.Contains`, **per PodClique reconcile** | **Yes — the O(N²) core** |
| `internal/controller/podcliqueset/components/podclique/podclique.go:110` | only uses `len()` of result | builds full slice for a count |
| `internal/controller/podclique/register.go:210` | `mapPodCliqueSetToPCLQs` event mapper, per PCS event | Yes |
| `internal/controller/podcliqueset/components/podcliquesetreplica/rollingupdate.go:254` | only uses `len()` | builds full slice for a count |

**Fix patterns already present in the tree** (not applied at the hot sites):
- `WithPodCliqueSetCache` / `GetPodCliqueSet` per-reconcile memoization —
  `podcliqueset.go:63-88`.
- `componentutils.NewSet(...)` + `.Has(...)` set-membership — used correctly at
  `podcliqueset/components/podclique/podclique.go:131,151`.

---

## Finding 2 — Unbounded goroutine fan-out on the client rate limiter

**Profile evidence (10x deploy goroutine, `-top`):** 11,661 total goroutines; **10,208 (87.54%)**
parked in:
```
service._resource.Sync.func1
  → doCreateOrUpdate
  → controllerutil.CreateOrPatch → client.Patch → rest.Request.Do
  → golang.org/x/time/rate.(*Limiter).WaitN → gopark
```
10x delete goroutine profile: 7,767 total, **6,306 (81%)** parked in the same
`service.Sync.func1` stack. This tracks load (deploy 10,208 → delete 6,306) → **not a leak**, but
a throughput/memory ceiling.

**Root cause:** `service._resource.Sync`
(`internal/controller/podcliqueset/components/service/service.go:76-97`) builds one task per PCS
replica (5,000 at 10x) and calls `utils.RunConcurrently`, which is
`RunConcurrentlyWithBounds(..., bound = len(tasks))` (`internal/utils/concurrent.go:91-92`) —
**effectively unbounded**. All tasks then serialize on the shared client QPS limiter (100 QPS /
150 burst, `manager.go:151-152`). Deploy time is therefore gated by `objects ÷ QPS`, and thousands
of goroutines just queue behind the limiter (each holding a stack + captured PCS).

**Fix pattern already present:** `RunConcurrentlyWithSlowStart` (`concurrent.go:70`, "prevents
overwhelming kube-apiserver") is already used by pod syncflow
(`podclique/components/pod/syncflow.go:268,372,533`) and HPA (`hpa/hpa.go:90`).

**Unbounded `RunConcurrently` call sites that fan out per-replica/per-object** (`grep`, non-test):
- `podcliqueset/components/service/service.go:88` ← the one in the profile
- `podcliqueset/components/podclique/podclique.go:161,176,260`
- `podcliqueset/components/resourceclaim/resourceclaim.go:124`
- `podcliqueset/components/podcliquescalinggroup/podcliquescalinggroup.go:137`
- `podcliquescalinggroup/components/podclique/sync.go:231,261`
- `podcliquescalinggroup/components/podclique/podclique.go:151,169` (delete paths)

QPS/Burst (100/150) in `manager.go:148-157` is the other lever for large deployments.

---

## Finding 3 — Patch churn dominates steady-state CPU (added here, not in prior analysis)

**Profile evidence (10x steady-state reconcile CPU, `-top -cum`):**
```
7.39s  52.12%  controllerutil.CreateOrPatch
2.76s  19.46%  runtime.(*unstructuredConverter).ToUnstructured
2.74s  19.32%  runtime.structToUnstructured
2.12s  14.95%  reflect.DeepEqual
2.10s  14.81%  reflect.deepValueEqual
2.30s  16.22%  service._resource.Sync.func1   ← still fanning out per-replica at steady state
```
**Root cause:** every reconcile calls `controllerutil.CreateOrPatch` on each managed object, which
round-trips the object through unstructured conversion (`ToUnstructured`) and a full
`reflect.DeepEqual`, **even when nothing changed**. This per-object overhead is multiplied by the
reconcile pressure from Finding 1. Note `service.Sync` still spawns per-replica CreateOrPatch
tasks (16%) at steady state even when no Service needs changing — Finding 2's fan-out and a
no-op short-circuit both reduce this.

Also visible on the deploy CPU profile (`-cum`): `client.Patch` 7.32%, `ToUnstructured` 7.32%
cum — the same path, under deploy load.

---

## Finding 4 — Memory dominated by informer-cache decode incl. managedFields

**Profile evidence (10x deploy memory, `inuse_space -top -cum`):** total in-use 1,212 MB.
```
731.05MB  60.30%  watch.(*StreamWatcher).receive        (informer watch stream decode)
393.69MB  32.47%  core/v1.(*Pod).Unmarshal
254.17MB  20.96%  meta/v1.(*ObjectMeta).Unmarshal
154.15MB  12.71%  meta/v1.(*ManagedFieldsEntry).Unmarshal
142.15MB  11.72%  meta/v1.(*FieldsV1).Unmarshal
222.16MB  18.32%  service._resource.Sync.func1 → CreateOrPatch   (patch-path live objects)
```
**Root cause / lever:** caching 10k pods scales linearly and is expected. But `managedFields`
(`FieldsV1` + `ManagedFieldsEntry`, ~12–14% combined) is a large, usually-unneeded chunk of cache
heap. The cache (`internal/controller/manager.go:129-145`) already restricts core types by label
selector (good) but sets **no `Transform`** to strip managedFields. A cache transform clearing
`ManagedFields` is the lever. Lower priority — linear, mostly expected.

---

## Ruled out (not independent problems)

- **GC/runtime at 40–50% of CPU** across phases (`mallocgc`, `scanSpan`, `tryDeferToSpanScan`,
  `spanClass.sizeclass`, `gcBgMarkWorker`) is a *symptom* of the allocation rate from Findings 1
  and 3, not a separate issue.
- **Goroutine counts are not a leak** — they track load (deploy 11,645 → steady 9,708 →
  delete 7,750).
- **Scale-down slowness (~14x).** The 10x ScaleDown_ToZero scale-down CPU profile (`-cum`) is
  dominated by watch-stream decode (`StreamWatcher.receive` 20.57%, `watch.Decoder.Decode`
  19.23%, protobuf/json decode), HTTP/2 read loop, and GC — i.e. informer catch-up + apiserver/etcd
  delete churn, **not** a Grove-side super-linear hot loop. The app-side scale-down cost rides on
  the same `CreateOrPatch` / fan-out paths as Findings 2 and 3.

---

## Recommended priority

1. **Finding 1 — O(N²) FQN membership** (`reconcilespec.go:113` + the builder). Biggest, cheapest
   deploy win; directly attacks the super-linear deploy time. Fix patterns already exist in-tree.
2. **Finding 2 — bound the fan-out** (`service.go:88` et al.) / tune QPS. Removes the
   10k-goroutine throughput ceiling.
3. **Finding 3 — patch churn.** Largely subsides once Finding 1 is fixed; a no-op short-circuit
   before `CreateOrPatch` helps steady state.
4. **Finding 4 — strip managedFields** from the cache (`manager.go` cacheOptions). Cuts
   steady-state heap; lower priority.

## Not yet drilled to line level

- ScaleUp / ScaleUp_FromZero 10x phase profiles (broadly the same deploy paths).
- 1x profiles line-by-line (used only as the linear baseline for the scaling ratios).
