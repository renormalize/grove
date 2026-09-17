# Grove Operator Scale Tests — Test Matrix & Resource Usage

**Date:** 2026-09-17 · **Branch:** `scale`
**Source:** 14 suite runs under `operator/diag/scale-{flat,disagg}-*`.
**Operator image:** `registry:5001/grove-operator:E2E_TESTS@sha256:f969cf46…` (same across all runs).

This document describes **what the scale tests exercise** and **how much resource each component
consumed** during the runs. It reports metrics only — no hotspot or root-cause analysis (see
`scale-report-flat-vs-disagg.md` for that).

---

## 1. The tests

The suite (`operator/e2e/tests/scale/`, driven by `hack/run-scale-suite.sh`) deploys a synthetic
workload against a k3d cluster with KWOK virtual nodes, then measures three phases:

- **deploy** — apply the workload, wait until all pods are Ready.
- **steady-state-reconcile** — trigger one no-op reconcile on every PodCliqueSet, hold a 30 s window.
- **delete** — delete the workload.

(`Test_ScaleUp*` / `Test_ScaleDown*` variants also run; this doc's metrics are whole-run peaks and
means, which span all phases.)

Resource usage is sampled on an interval into two CSVs per run:
- `usage-pods.csv` — per-pod CPU (millicores) and memory (bytes) for the `grove-system`,
  `kai-scheduler`, `kube-system`, and `pyroscope` namespaces.
- `usage-server.csv` — the k3d server container (the k3s control plane: apiserver + etcd + KCM +
  scheduler), via `docker stats` (CPU as % of one core, so 100% = 1 core).

---

## 2. Three test dimensions

Total pod count is held **constant** at each scale across shape and spread, so all points are
directly comparable.

| Axis | Knob | Values run | Effect |
|---|---|---|---|
| **Scale** | `SCALE_PCS_REPLICAS` | 1x, 10x | 1,000 / 10,000 total pods |
| **Shape** | `SCALE_WORKLOAD` | flat, disagg | workload topology (below) |
| **Spread** | `SCALE_PCS_COUNT` | 1, 10, 50, 100 | one wide PodCliqueSet vs. N smaller ones |

14 combinations were run (`disagg / 1x / pcs100` is auto-skipped — too few replicas to spread across
100 PodCliqueSets).

### 2.1 Shape: flat vs disagg

**flat** — the simple baseline. One standalone PodClique of **2 pods** per PCS replica.

```
PodCliqueSet
  └─ PodClique (standalone)  →  2 pods per replica
```
Pods = `PCS.replicas × 2`. At 10x/pcs1 that is 5,000 replicas × 2 = 10,000 pods.

**disagg** — a realistic disaggregated LLM-serving topology. Each PCS replica is one model-serving
instance split into a **prefill** and a **decode** PodCliqueScalingGroup (decode-heavy, 1:2 ratio),
each group tensor-parallel-sized (prefill TP=4 pods, decode TP=8 pods):

```
PodCliqueSet
  ├─ prefill PCSG  ×1   →  prefill-worker PodClique, TP=4 pods
  └─ decode  PCSG  ×2   →  decode-worker  PodClique, TP=8 pods
```
Pods per PCS replica = `prefillPCSG×4 + decodePCSG×8`. Both shapes total **1000 × multiplier** pods.

| Tier | flat | disagg |
|---|---|---|
| **1x** (1,000 pods) | 500 PCS replicas × 2 pods | 50 PCS × (1 prefill×4 + 2 decode×8) = 50 × 20 |
| **10x** (10,000 pods) | 5,000 PCS replicas × 2 pods | 100 PCS × (5 prefill×4 + 10 decode×8) = 100 × 100 |

### 2.2 Spread: `SCALE_PCS_COUNT`

The same total pods are split across **N separate PodCliqueSet objects** (each with `1/N` of the
replicas). `pcs1` = one wide PodCliqueSet; `pcs10/50/100` = the workload fanned across 10/50/100
smaller PodCliqueSets. Total pods and the flat/disagg shape are unchanged by N.

### 2.3 Scale (pods per run)

| Run group | Total pods |
|---|---|
| all **1x** runs (flat & disagg, any spread) | ~1,000 |
| all **10x** runs (flat & disagg, any spread) | ~10,000 |

---

## 3. Cluster components measured

| Component | Namespace / source | What it is |
|---|---|---|
| **grove-operator** | `grove-system` | the operator under test |
| **k3d server** | `usage-server.csv` | k3s control plane — apiserver, etcd, KCM, scheduler |
| **kai-scheduler** | `kai-scheduler` | scheduler stack (admission, binder, pod-grouper, queue/podgroup controllers, scheduler) |
| **kube-system** | `kube-system` | coredns, kwok-controller, metrics-server, traefik, local-path-provisioner |
| pyroscope | `pyroscope` | profiling sidecar (support infra, omitted from tables below) |

CPU is reported in millicores (`m`) for pods; the k3d server is `docker stats` CPU % of one core
(2400% ≈ 24 cores). Namespace figures are per-timestamp totals across all pods in that namespace.

---

## 4. Resource usage — 10x runs (~10,000 pods)

### grove-operator

| Run | CPU mean (m) | CPU max (m) | Mem mean (MB) | Mem max (MB) |
|---|---|---|---|---|
| flat 10x pcs1   | 422 | 1,369 | 1,285 | **2,002** |
| flat 10x pcs10  | 145 | 1,117 | 979 | 1,257 |
| flat 10x pcs50  | 139 | 1,058 | 796 | 1,064 |
| flat 10x pcs100 | 147 | 1,051 | 745 | 986 |
| disagg 10x pcs1   | 1,050 | 2,847 | 903 | 1,571 |
| disagg 10x pcs10  | 439 | 2,964 | 607 | 779 |
| disagg 10x pcs50  | 447 | **3,567** | 554 | 731 |
| disagg 10x pcs100 | 392 | 3,064 | 550 | 721 |

- Operator memory peaks at the **single-PCS (pcs1)** point for both shapes (flat 2.0 GB, disagg
  1.6 GB) and falls as the workload is spread across more PodCliqueSets.
- Operator CPU peaks are higher for **disagg** (2.8–3.6 cores) than flat (1.0–1.4 cores).

### k3d server (control plane: apiserver + etcd + KCM + scheduler)

| Run | CPU mean (%) | CPU max (%) | Mem mean (GB) | Mem max (GB) |
|---|---|---|---|---|
| flat 10x pcs1   | 725 | 2,422 | 10.3 | **16.7** |
| flat 10x pcs10  | 322 | 2,335 | 9.6 | 11.5 |
| flat 10x pcs50  | 320 | 1,931 | 8.7 | 10.8 |
| flat 10x pcs100 | 331 | 1,646 | 8.9 | 11.2 |
| disagg 10x pcs1   | 752 | 2,423 | 8.5 | 13.2 |
| disagg 10x pcs10  | 399 | 1,590 | 8.8 | 10.6 |
| disagg 10x pcs50  | 397 | 1,634 | 8.9 | 11.1 |
| disagg 10x pcs100 | 404 | 1,669 | 8.9 | 11.0 |

- The control plane is the **heaviest single component** at 10x: **~11–17 GB** memory and CPU peaks of
  **~16–24 cores** (2422% ≈ 24 cores).

### kai-scheduler & kube-system (namespace totals)

| Run | kai CPU max (m) | kai Mem max (MB) | kube CPU max (m) | kube Mem max (MB) |
|---|---|---|---|---|
| flat 10x pcs1   | 207 | 759 | 2,532 | **1,060** |
| flat 10x pcs10  | 227 | 808 | 1,622 | 840 |
| flat 10x pcs50  | 376 | 808 | 2,632 | 847 |
| flat 10x pcs100 | 271 | 871 | 2,603 | 804 |
| disagg 10x pcs1   | 307 | 694 | 2,823 | 729 |
| disagg 10x pcs10  | 306 | 712 | 772 | 666 |
| disagg 10x pcs50  | 429 | 665 | 1,546 | 644 |
| disagg 10x pcs100 | 230 | 752 | 1,291 | 647 |

- kai-scheduler holds ~0.7–0.9 GB and stays under ~0.4 core at 10x.
- kube-system CPU spikes (up to ~2.6 cores) come largely from the KWOK controller materializing
  virtual pods; memory ~0.6–1.1 GB.

---

## 5. Resource usage — 1x runs (~1,000 pods)

### grove-operator

| Run | CPU mean (m) | CPU max (m) | Mem mean (MB) | Mem max (MB) |
|---|---|---|---|---|
| flat 1x pcs1  | 365 | 760 | 206 | 284 |
| flat 1x pcs10 | 136 | 864 | 176 | 207 |
| flat 1x pcs50 | 128 | 830 | 155 | 181 |
| disagg 1x pcs1  | 403 | 1,173 | 133 | 178 |
| disagg 1x pcs10 | 224 | 1,129 | 122 | 153 |
| disagg 1x pcs50 | 215 | 1,103 | 133 | 157 |

- Operator memory at 1x is **~150–285 MB** (roughly 7–10× lower than the 10x runs).
- CPU peaks ~0.8 core (flat) to ~1.2 cores (disagg).

### k3d server

| Run | CPU mean (%) | CPU max (%) | Mem mean (GB) | Mem max (GB) |
|---|---|---|---|---|
| flat 1x pcs1  | 292 | 835 | 3.4 | 4.2 |
| flat 1x pcs10 | 156 | 783 | 3.3 | 3.6 |
| flat 1x pcs50 | 154 | 861 | 3.2 | 3.5 |
| disagg 1x pcs1  | 356 | 938 | 2.8 | 3.7 |
| disagg 1x pcs10 | 234 | 892 | 2.9 | 3.3 |
| disagg 1x pcs50 | 222 | 923 | 3.0 | 3.3 |

- Control plane at 1x: **~3–4 GB** memory, CPU peaks ~8–9 cores.

### kai-scheduler & kube-system (namespace totals)

| Run | kai CPU max (m) | kai Mem max (MB) | kube CPU max (m) | kube Mem max (MB) |
|---|---|---|---|---|
| flat 1x pcs1  | 180 | 513 | 300 | 218 |
| flat 1x pcs10 | 159 | 381 | 259 | 212 |
| flat 1x pcs50 | 145 | 369 | 238 | 223 |
| disagg 1x pcs1  | 201 | 400 | 460 | 184 |
| disagg 1x pcs10 | 198 | 392 | 316 | 195 |
| disagg 1x pcs50 | 182 | 389 | 310 | 199 |

---

## 6. Summary of resource scaling

| Component | 1x (~1k pods) | 10x (~10k pods) |
|---|---|---|
| grove-operator memory (max) | 150–285 MB | 720 MB – 2.0 GB |
| grove-operator CPU (max) | 0.8–1.2 cores | 1.0–3.6 cores |
| k3d control plane memory (max) | 3–4 GB | **11–17 GB** |
| k3d control plane CPU (max) | ~8–9 cores | **~16–24 cores** |
| kai-scheduler memory (max) | 0.4–0.5 GB | 0.6–0.9 GB |
| kube-system CPU (max) | ~0.3 core | up to ~2.6 cores |

- Memory scales roughly with pod count for the operator and control plane (~7–10× from 1x to 10x).
- The **k3d control plane is consistently the largest resource consumer** at both scales.
- Within a scale, the **single-wide-PCS (pcs1)** layout uses the most operator and control-plane
  memory; spreading across more PodCliqueSets lowers both.

---

## 7. Total cluster resource consumption

Whole-cluster totals per run = grove-operator + k3d control plane + kai-scheduler + kube-system
(pyroscope excluded). Computed **per sample timestamp** and then reduced to mean/max, so the total
max is the peak simultaneous cluster load — it is **lower than the sum of each component's individual
max**, because component peaks do not all occur at the same instant.

| Run | Total CPU mean (cores) | Total CPU max (cores) | Total Mem mean (GB) | Total Mem max (GB) |
|---|---|---|---|---|
| flat 10x pcs1   | 1.5 | 5.6 | 12.6 | **19.7** |
| flat 10x pcs10  | 0.7 | 3.6 | 11.6 | 13.6 |
| flat 10x pcs50  | 0.7 | 3.2 | 10.6 | 12.8 |
| flat 10x pcs100 | 0.7 | 3.2 | 10.7 | 12.9 |
| disagg 10x pcs1   | 2.4 | **6.5** | 10.2 | 15.6 |
| disagg 10x pcs10  | 1.1 | 4.9 | 10.3 | 12.4 |
| disagg 10x pcs50  | 1.1 | 5.0 | 10.3 | 12.5 |
| disagg 10x pcs100 | 1.1 | 4.8 | 10.3 | 12.5 |
| flat 1x pcs1    | 0.9 | 1.6 | 4.1 | 5.2 |
| flat 1x pcs10   | 0.4 | 1.8 | 3.9 | 4.3 |
| flat 1x pcs50   | 0.4 | 1.4 | 3.8 | 4.2 |
| disagg 1x pcs1    | 1.0 | 2.3 | 3.4 | 4.4 |
| disagg 1x pcs10   | 0.7 | 1.7 | 3.5 | 4.0 |
| disagg 1x pcs50   | 0.6 | 1.8 | 3.6 | 4.0 |

- Peak whole-cluster memory: **~4–5 GB at 1x**, **~12–20 GB at 10x**. The k3d control plane is the
  bulk of this (§4/§5).
- Peak whole-cluster CPU: **~1.4–2.3 cores at 1x**, **~3.2–6.5 cores at 10x**.
- The **single-wide-PCS (pcs1)** layout is the heaviest total-consumption point at each scale; the
  `flat 10x pcs1` run is the overall memory peak (~19.7 GB) and `disagg 10x pcs1` the overall CPU peak
  (~6.5 cores).

*Metric provenance: `<run>/usage-pods.csv` (grove-operator row + per-namespace totals),
`<run>/usage-server.csv` (k3d server). Means/maxes computed over all interval samples per run.*
