# Grove Operator Scale Test — Flat vs Disagg, Spread Sweep

**Date:** 2026-09-17 · **Branch:** `scale`
**Runs analyzed:** 14 suite runs under `operator/diag/scale-{flat,disagg}-*` (not checked in).
**Operator image:** `registry:5001/grove-operator:E2E_TESTS@sha256:f969cf46…` (same across all runs).
**Client config:** QPS=100, Burst=150; controller concurrency = 20 each (PCS / PCSG / PCLQ).

This report covers the new experimental axes added on this branch — **workload shape**
(`flat` vs `disagg`) and **PCS spread** (`SCALE_PCS_COUNT`) — on top of scale (`1x`/`10x`).
It builds on the prior single-shape work in `scale-analysis.md` and `scale-hotspots-analysis.md`
(those analyzed an earlier `scale-1x`/`scale-10x` run and found the O(N²) FQN rebuild, the
unbounded service fan-out, patch churn, and managedFields cache cost — all of which reappear
here and are cross-referenced below).

Timings come from each run's `scale-test-results.json`; resource peaks from `usage-pods.csv` /
`usage-server.csv`; hotspots from `go tool pprof` on the per-phase `*.pprof.gz`.

---

## 1. What was run

Three orthogonal knobs, **total pod count held constant** at each scale so points are comparable:

| Axis | Knob | Values here | Meaning |
|---|---|---|---|
| Scale | `SCALE_PCS_REPLICAS` | 1x (1,000 pods), 10x (10,000 pods) | total pods = 1000 × mult |
| Shape | `SCALE_WORKLOAD` | `flat`, `disagg` | standalone cliques vs. prefill+decode PCSGs |
| Spread | `SCALE_PCS_COUNT` | 1, 10, 50, 100 | one wide PodCliqueSet vs. N smaller ones |

- **flat**: one standalone PodClique of 2 pods per PCS replica. 10x/pcs1 ⇒ 5,000 replicas × 2 = 10k pods.
- **disagg**: per PCS replica, a prefill PCSG + a decode PCSG (1:2 ratio), tensor-parallel-sized
  (prefill TP=4, decode TP=8). Tiers: 1x = 50 PCS ×(1×4 + 2×8)=1k; 10x = 100 PCS ×(5×4 + 10×8)=10k.
- **spread**: the same total pods split across N separate PodCliqueSet objects. `disagg/1x/count=100`
  is intentionally skipped (too few replicas to spread) — hence 14 runs, not 16.

Each run measures three phases in `Test_ScaleTest` — **deploy → steady-state no-op reconcile →
delete** — plus ScaleUp/ScaleDown variants, and captures CPU/memory/goroutine pprof once per phase.

> **⚠️ Methodology caveat that governs the whole "delete" column — read this first.**
> The single-PCS and multi-PCS code paths **do not measure the same delete event.**
> - Single-PCS (`pcs1`) stops the clock at the **`pcs-deleted`** milestone, which fires as soon as
>   the PodCliqueSet object returns `NotFound` (`condition/pcs.go:37`). With background cascade
>   deletion that is a single API round-trip — **~0.1 s regardless of how many pods exist.** It does
>   **not** wait for pods to terminate.
> - Multi-PCS (`pcs10/50/100`) stops at the **`pods-deleted`** milestone, which polls until the live
>   pod count actually reaches zero (`condition/pod.go:152`).
>
> So the pcs1 "0.1 s delete" and the pcs10 "900–2100 s delete" are **not comparable** — pcs1 is not
> faster at reclaiming pods, it just declares victory earlier. The genuine finding in the delete data
> is about *pod-teardown throughput* (the multi-PCS numbers), not about spread making delete slower.

---

## 2. Headline results

### 2.1 `Test_ScaleTest` deploy → pods-ready (seconds)

| | 1x pcs1 | 1x pcs10 | 1x pcs50 | 10x pcs1 | 10x pcs10 | 10x pcs50 | 10x pcs100 |
|---|---|---|---|---|---|---|---|
| **flat**  | 55 | 45 | 44 | **945** | 450 | 465 | 487 |
| **disagg**| 31 | 30 | 30 | **309** | 310 | 301 | 293 |

- **Disagg deploys ~3× faster than flat at 10x** (309 s vs 945 s for pcs1) despite identical total
  pods. This is the single largest shape effect. **Root cause in §3.**
- **Spread barely affects deploy.** Splitting the workload across 10/50/100 PCS changes deploy-ready
  time by <10% within a shape. (flat/10x is actually *faster* split — 450 s at pcs10 vs 945 s at
  pcs1 — because splitting reduces the per-PCS O(N²) fan-out; see §3.)

### 2.2 `Test_ScaleTest` delete (seconds) — *see caveat above*

| | 1x pcs1† | 1x pcs10 | 1x pcs50 | 10x pcs1† | 10x pcs10 | 10x pcs50 | 10x pcs100 |
|---|---|---|---|---|---|---|---|
| **flat**  | 0.1 | 214 | 231 | 31† | **2122** | 2138 | 2178 |
| **disagg**| 0.1 | 76 | 85 | 0.1† | **912** | 891 | 902 |

† pcs1 measures only `pcs-deleted` (object gone), **not** pod teardown — not comparable to the
multi-PCS columns. The real signal is the multi-PCS pod-teardown time.

- **Actual pod teardown is slow and grows with total pods, not with spread**: at 10x, flat drains
  ~10k pods in ~2,100 s (~4.7 pods/s) and disagg in ~900 s (~11 pods/s), roughly flat across
  pcs10/50/100. **Disagg drains ~2.3× faster than flat. Root cause in §4 — it is *not* the operator.**

### 2.3 Resource peaks

| Run | operator CPU (m) | operator mem (MB) | k3d server CPU (%) | k3d server mem (GB) |
|---|---|---|---|---|
| disagg 10x pcs1  | 2847 | **1571** | 2423 | 13.2 |
| disagg 10x pcs10 | 2964 | 779 | 1590 | 10.6 |
| flat 10x pcs1    | 1369 | **2002** | 2422 | **16.7** |
| flat 10x pcs10   | 1117 | 1257 | 2335 | 11.5 |
| flat 10x pcs100  | 1051 | 986  | 1646 | 11.2 |
| disagg 1x (all)  | ~1150 | ~160 | ~910 | ~3.5 |
| flat 1x (all)    | ~820 | ~220 | ~830 | ~3.7 |

- **Operator memory peaks in the pcs1 (single wide PCS) case** for both shapes (flat 2.0 GB, disagg
  1.6 GB) and *drops* as you spread across more PCS. The wide-PCS reconcile holds the most live state
  at once. Memory is dominated by the informer cache, not live work objects (§5).
- **Operator CPU is higher for disagg** (2.8–3.6 cores) than flat (1.0–1.4 cores) at 10x — disagg
  does its work *hotter and faster*; flat idles on the rate limiter (§3).
- The **k3d server (apiserver + etcd + KCM + scheduler)** peaks at **~15–17 GB / ~24 cores** at 10x —
  the control plane, not the operator, is the heaviest single component at scale.

---

## 3. Finding — Flat deploy is 3× slower because it *idle-waits on the client rate limiter*, not because it computes

At 10x the deploy CPU profiles tell the story directly:

| Profile | Duration | Sampled CPU-s | Sampling % (≈ cores busy) |
|---|---|---|---|
| **flat 10x pcs1** | 944.6 s | 492 s | **52%** (~0.5 core) |
| flat 10x pcs10 | 450 s | 231 s | 51% |
| **disagg 10x pcs1** | 309 s | 511 s | **165%** (~1.65 cores) |
| disagg 10x pcs10 | 310 s | 451 s | 145% |

Flat runs 3× longer while its CPU sits at ~0.5 core; disagg finishes in a third of the time while
busy across ~1.65 cores. **Flat is not CPU-bound — it is blocked.**

The deploy **goroutine** profiles pin the block:

| | Live goroutines | Dominant parked stack |
|---|---|---|
| **flat 10x pcs1** | **9,362** | **7,838 (84%) in `rate.(*Limiter).WaitN`** under `service._resource.doCreateOrUpdate` |
| **disagg 10x pcs1** | **1,514** | no rate-limiter concentration; scattered across normal I/O decode/poll |

Flat's topology fans out **one headless Service create/patch per PCS replica** (5,000 at 10x/pcs1),
all firing concurrently through the shared client-go limiter (100 QPS / 150 burst). ~84% of ~9,300
goroutines just park in `WaitN` waiting for tokens — deploy is gated by *objects ÷ QPS*, not by CPU.
This is exactly the **unbounded service fan-out** described in `scale-hotspots-analysis.md` Finding 2,
now shown to be shape-specific: **flat triggers it; disagg does not** (disagg's writes go through the
podgang path with far fewer concurrent client calls).

**Why splitting flat across more PCS makes deploy faster, not slower** (945 s→450 s): the per-PCS
O(N²) FQN rebuild (below) shrinks quadratically with per-PCS replica count, so 10 PCS of 500 replicas
each is cheaper than 1 PCS of 5,000.

### The flat operator CPU that *is* burned = the O(N²) FQN rebuild (`scale-analysis.md` Finding 1, reconfirmed)

Of flat pcs1's ~52% CPU, the reconcile portion is dominated by name generation:
`podclique.Reconcile` (24% cum) → `shouldCheckPendingUpdatesForPCLQ` (14%) →
`GetPodCliqueFQNsForPCSNotInPCSG` (13.9%) → `GeneratePodCliqueName` (11.3%) → `fmt.Sprintf` (9.8%).
`namegen.go:53` alone = **56 s cumulative**. The FQN name-set is rebuilt from scratch (Sprintf per
name) on every PodClique reconcile and scanned with `slices.Contains` — O(N²) Sprintf calls. Plus the
GC that allocation storm drives (~26%).

**Disagg's deploy hot path is completely different** — the FQN rebuild is essentially absent
(`fmt.Sprintf` only 2.6%). Disagg CPU goes to `podgang.Sync` → `createOrUpdatePodGang` (24% cum),
`CacheReader.List` + `mapaccess2_faststr` (17–24%), and `CreateOrPatch`/`ToUnstructured` (19%) — real
forward-progress reconcile work done at high concurrency.

---

## 4. Finding — Multi-PCS delete is slow because of *apiserver/etcd teardown + an idle reconcile queue*, not the operator

Delete of ~10k pods takes 2,122 s (flat) / 912 s (disagg). The operator is **almost entirely idle**
the whole time:

| Delete profile | Duration | Sampled CPU-s | Sampling % |
|---|---|---|---|
| flat 10x pcs10 | 2122 s | 75 s | **3.5%** |
| disagg 10x pcs10 | 912 s | 29 s | **3.1%** |
| flat 1x pcs10 | 214 s | 4 s | **1.9%** |

`profiler.log` independently confirms it: grove-system CPU **mean 145 m (0.145 core)** over the
flat-10x-pcs10 delete window. The little CPU spent is **GC + informer watch-stream decode**
(`StreamWatcher.receive` → `Decoder.Decode` → json ≈ 15–19% cum) — i.e. the operator passively
receiving delete events — plus, tellingly, ~10% is the pprof endpoint profiling itself, which is only
possible because nothing else is running.

The delete **goroutine** profiles are the clincher. In every slow multi-PCS run, **all 268 reconcile
workers are parked on an empty workqueue**:
`processNextWorkItem → priorityqueue.GetWithPriority → selectgo → gopark`. **Zero** goroutines are in
`rate.Limiter.WaitN`, `client.Delete`, or a Grove reconcile body. The operator is **not**
rate-limited, **not** blocked on the apiserver, and **not** computing — it is *waiting for work to be
enqueued*.

Ruling out each candidate cause:
- **Not operator CPU** — 3.5% / 3.1% / 1.9% sampled; 145 m mean.
- **Not the client QPS limiter** — the slow runs have *zero* goroutines in `WaitN`. The one run that
  heavily uses the limiter (flat-10x-pcs1, 8,551 goroutines in `WaitN`) is the *fast* 30 s case.
- **Consistent with apiserver/etcd teardown throughput + reconcile-requeue latency** — the wall-clock
  is spent *between* watch events (etcd deleting objects, informer relaying), with the reconcile queue
  empty in the gaps.

**Not a goroutine leak.** Within one run goroutines drain cleanly by ~9×:
steady-state 13,653 → deploy 9,141 → delete 1,512. The ~1,512 floor (268 idle workers + 76
reflector-sets) is a fixed idle baseline identical across flat/disagg/1x — load-tracking, not
accumulation.

**Open sub-question** (not resolvable from CPU/goroutine profiles alone): whether the between-events
idle is pure etcd/apiserver delete throughput, or a Grove requeue/backoff interval leaving the queue
empty between per-PCS delete cascades. Both produce the identical signature (idle CPU + 268 workers
parked on an empty priorityqueue). Distinguishing them needs workqueue depth/latency metrics or the
reconcile `RequeueAfter` values — worth capturing in a future run. Disagg draining 2.3× faster than
flat for the same pod count hints the operator-side requeue cadence *does* matter (fewer, TP-grouped
objects to walk), so (d) is plausible and cheap to check.

---

## 5. Finding — Steady-state & memory: patch churn + managedFields, as before, now split by shape

### Steady-state no-op reconcile CPU (share of sampled CPU)

| | patch churn (CreateOrPatch: ToUnstructured + DeepEqual) | GC | dominant fan-out path |
|---|---|---|---|
| flat pcs1 | ~1.5% | 25% | **FQN Sprintf 29%** (`GetPodCliqueFQNsForPCSReplicaNotInPCSG` → `GeneratePodCliqueName`) |
| flat pcs10 | **~23%** | 33% | `service._resource.Sync.func1` (per-PodClique services) |
| disagg pcs1 | ~26% | 39% | `podgang._resource.Sync` → `createOrUpdatePodGang` |
| disagg pcs10 | ~14% | 47% | `podgang.Sync` + `informerCache.List`→`getExistingPodsByPCLQForPCS` (20%) |

- On **no-op** reconciles, `CreateOrPatch` still round-trips every object through
  `ToUnstructured` + `reflect.DeepEqual` even when nothing changed — that ~14–26% is wasted diff work
  (matches `scale-hotspots-analysis.md` Finding 3). A no-op short-circuit before `CreateOrPatch` would
  remove it.
- **flat pcs1 is the outlier**: its steady-state cost is *not* patch churn but the same FQN Sprintf
  rebuild seen at deploy — recomputed on every reconcile.
- The **fan-out path is shape-specific**: flat → `service.Sync` (per-clique services);
  disagg → `podgang.Sync` + a per-podgang `informerCache.List` that walks cached pods
  (`getExistingPodsByPCLQForPCS`, `mapaccess2_faststr`).
- GC is the largest single consumer in 3 of 4 (up to 47%), fed by the `ToUnstructured`
  `map[string]interface{}` allocations and the flat-pcs1 Sprintf storm.

### Deploy memory (inuse_space, 10x pcs1) — heap is the informer cache, ~1 GB either way

| | total heap | informer watch-decode (`StreamWatcher.receive`) | List→DeepCopy | CreateOrPatch live objs |
|---|---|---|---|---|
| flat pcs1 | 1121 MB | **843 MB (75%)** | negligible | 70 MB (6%) |
| disagg pcs1 | 1059 MB | 540 MB (51%) | **312 MB (29%) `Pod.DeepCopy`** | 11 MB (1%) |

- Heap is dominated by the **informer cache decoding pods** (`Pod.Unmarshal`, `ObjectMeta.Unmarshal`),
  not by live reconcile objects — expected, scales linearly with pod count.
- **managedFields is a large, consistent tax** (`scale-hotspots-analysis.md` Finding 4, reconfirmed):
  `FieldsV1` + `ManagedFieldsEntry` ≈ **19–21% of total heap** in both shapes. Disagg pays it **twice**
  — once decoding the watch stream, once deep-copying it out of the cache on every per-podgang `List`
  (`FieldsV1.DeepCopyInto` +108 MB). A cache transform stripping `ManagedFields` is the lever, and it
  helps disagg more.
- **Disagg-specific pressure**: `podgang.Sync` calls `informerCache.List` →
  `getExistingPodsByPCLQForPCS` → `Pod.DeepCopy` (312 MB, 29% of heap) on every reconcile — deep copies
  of cached pods per podgang. This is why disagg's *hot-path* CPU shows heavy `List`/`mapaccess`.

---

## 6. So what — takeaways by axis

**Shape (flat vs disagg) matters more than anything else at scale:**
- **flat** is bottlenecked on the **client rate limiter** during deploy (unbounded per-replica service
  fan-out) and carries the **O(N²) FQN Sprintf** cost — both make flat/10x deploy 3× slower than
  disagg while leaving the CPU half-idle. Bounding the service fan-out (`RunConcurrentlyWithSlowStart`
  already exists in-tree) and fixing the FQN rebuild are the flat-specific wins.
- **disagg** avoids both, but pays a per-podgang **`informerCache.List` + `Pod.DeepCopy`** cost (CPU
  and ~29% of heap) and heavier patch churn. Its bottleneck is real work, done concurrently.

**Spread (PCS count) is mostly neutral for deploy**, *reduces* peak operator memory (wide PCS holds
the most state), and does **not** independently make delete slower — the multi-PCS "slow delete" is a
measurement artifact vs pcs1 (§ caveat) layered on genuine, pod-count-driven teardown latency.

**Delete/teardown is control-plane-bound, not operator-bound.** ~4.7 pods/s (flat) to ~11 pods/s
(disagg) at 10x, operator idle at <4% CPU, 268 workers parked on an empty queue. Improving it means
apiserver/etcd delete throughput and/or the operator's requeue cadence — profile workqueue latency
next to separate the two.

**Priority (net of prior analyses):**
1. **Bound the flat service fan-out / raise QPS for large deploys** — removes the 84%-parked-goroutine
   deploy ceiling that makes flat 3× slow. (`scale-hotspots-analysis.md` F2, now shown flat-specific.)
2. **Fix the O(N²) FQN rebuild** (`shouldCheckPendingUpdatesForPCLQ` + the builder) — flat deploy &
   steady-state. (`scale-analysis.md` F1.)
3. **Capture workqueue depth/latency in the delete phase** to resolve §4's etcd-vs-requeue question
   before optimizing teardown.
4. **No-op short-circuit before `CreateOrPatch`** — cuts 14–26% steady-state patch churn. (F3.)
5. **Strip `managedFields` via a cache transform** — ~20% of heap, and disagg pays it twice. (F4.)

---

## 7. Data provenance & gaps

- Timings: `<run>/ScaleTest/*/scale-test-results.json` (all 14 runs).
- Resource peaks: `<run>/usage-pods.csv` (grove-operator row), `<run>/usage-server.csv`, `profiler.log`.
- pprof: `go tool pprof` (go1.26.4) on `<run>/<phase>/*/pprof-*-{cpu,memory,goroutine}.pprof.gz`.
  Deep-dived: 10x deploy (flat/disagg × pcs1/pcs10), 10x delete (flat/disagg pcs10, flat pcs1, flat
  1x pcs10), 10x steady-state (4 profiles), 10x deploy memory (flat/disagg pcs1).
- **Gaps:** flat-10x-pcs10/50/100 runs are missing the `ScaleUp` (non-FromZero) phase — only
  `ScaleUp_FromZero`/`_Tiny` present. ScaleUp/ScaleDown pprof not line-level analyzed (broadly the
  same deploy/teardown paths). Workqueue depth/latency not captured (needed for §4). 50x/100x were not
  in this fetched set for either shape.
