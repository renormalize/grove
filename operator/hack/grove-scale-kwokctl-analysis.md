# Grove Scale Tests on kwokctl --runtime=kind — Analysis Summary

Date: 2026-08-03
Author: investigation for migrating grove operator scale tests off k3d onto kwokctl.

## 1. Goal

The grove operator scale tests (`operator/e2e/tests/scale/`) validate the operator's
reconciliation behavior at scale (many PodCliqueSets, many pods) against *fake* KWOK
nodes and pods — no real workload containers ever run. Today they run on a **k3d**
cluster, which drags in container-runtime overhead we don't need for a fake-node test.

Objective: run the same tests on **kwokctl** to shed that overhead, then push grove to
find node/PCS-workload limits.

## 2. Approach chosen (and why)

Two runtime options were on the table:

- **Operator as a host process + KWOK apiserver only.** Rejected: grove serves admission
  webhooks (mutating + validating on PodCliqueSet, plus ClusterTopology validation) and
  its cert-controller auto-provisions TLS keyed to the in-cluster DNS name
  `grove-operator.<ns>.svc`. Running the operator off-cluster means reworking cert
  provisioning and webhook wiring. KAI scheduler likewise expects to run in-cluster.

- **kwokctl `--runtime=kind` (CHOSEN).** kwokctl stands up a single real `kindest/node`
  container (real kubelet + containerd + control plane) and runs its built-in
  kwok-controller inside it. Real in-cluster components (grove-operator, KAI, kube-system)
  run unmodified on that one real node; the KWOK fake-node fleet carries the scale load.
  This keeps webhooks/certs/KAI working exactly as in production while still dropping most
  of the container footprint.

Decision sequence (user-confirmed):
1. Measure the k3d baseline overhead FIRST.
2. Prototype kwokctl `--runtime=kind` end-to-end by hand before touching any automation.

## 3. Tooling / versions (reproducibility)

| tool | version |
|------|---------|
| kwokctl | v0.8.0 (go1.26.4, darwin/arm64) |
| kind | 0.32.0 |
| kind node image | docker.io/kindest/node:v1.36.1 |
| skaffold | v2.24.0 |
| helm | v3.18.6 |
| KAI scheduler | v0.15.2 (oci://ghcr.io/kai-scheduler/kai-scheduler/kai-scheduler) |

Grove images built via skaffold's `ko` builder, profile `topology-test`, VERSION=E2E_TESTS.

## 4. Baseline measurement — k3d (scale.yaml preset)

### Method
- Brought up the existing scale cluster via `make scale-cluster-up` (infra_manager.py with
  `hack/scale.yaml`: worker_nodes=0, kwok.nodes=100, kai+grove+pyroscope+kwok-controller,
  profiling on).
- Counted nodes with `kubectl get nodes`, pods with `kubectl get pods -A`.
- Measured container memory/CPU with `docker stats --no-stream`.

### Metrics
- **Node topology:** 101 nodes total = 1 real k3d server + 100 KWOK fake.
- **Containers: 4**, total idle memory **~1.87 GiB**:

  | container | image | idle mem | idle CPU |
  |-----------|-------|----------|----------|
  | k3d-...-server-0 | rancher/k3s:v1.35.5-k3s1 | ~1.78 GiB | 13-44% (spiky) |
  | k3d-...-serverlb | k3d-proxy:5.9.0 | ~23 MiB | 0% |
  | k3d-...-tools | k3d-tools:5.9.0 | ~5 MiB | 0% |
  | registry | registry:2 | ~62 MiB | 0% |

- **In-cluster pods:** 18.
- The single k3s server container carries ALL load (embedded etcd + apiserver + kcm +
  scheduler + kwok-controller + grove + kai + pyroscope). Idle CPU is notably spiky
  (13-44%) even at rest with 100 kwok nodes.

### Incidental finding (real bug, runtime-independent)
`make scale-cluster-up` FAILED at KAI queue creation: the KAI admission webhook was not
ready inside the 12x5s retry window in `apply_kai_queues` (`kai.py`), so the run aborted
with `Failed to create Kai queues after retries`. Re-running `kubectl apply -f
e2e/yaml/queues.yaml` seconds later succeeded. The retry window is too short — worth
widening regardless of the kwokctl work.

## 5. Prototype — kwokctl --runtime=kind (cluster "grove-scale")

### Method (exact steps, in order)

1. **Create cluster:** `kwokctl create cluster --name grove-scale --runtime=kind`
   → single container `kwok-grove-scale-control-plane` (kindest/node:v1.36.1).
   Kubeconfig written to a temp path (stored in `/tmp/grove-scale-kcfg-path` for reuse).

2. **Uncordon the kind node:** the node came up cordoned
   (`node.kubernetes.io/unschedulable:NoSchedule`), which blocks all real pods.
   `kubectl uncordon kwok-grove-scale-control-plane` → node Ready & schedulable.
   Verified by scheduling a throwaway pause pod (ran successfully).

3. **Local registry for grove's own images.** grove-operator/initc/install-crds are REAL
   pods whose images must exist on the node (unlike fake workload pods, whose images are
   never pulled). k3d provides `registry:5001`; kwokctl kind does not. Set up:
   - `docker run -d --restart=always -p 127.0.0.1:5001:5000 --network kind --name
     kind-registry registry:2` (on the `kind` docker network so the node can reach it).
   - On the kind node, add a containerd registry mirror:
     `/etc/containerd/certs.d/localhost:5001/hosts.toml` pointing
     `[host."http://kind-registry:5000"]` with `skip_verify=true`; then
     `systemctl restart containerd` (node survived the restart, stayed Ready).
   - Verified the mirror with `crictl pull localhost:5001/grove-operator:...@sha256:...`
     on the node → pulled successfully via the redirect to kind-registry:5000.

4. **Build + deploy grove:**
   - `skaffold build -p topology-test --default-repo=localhost:5001
     --file-output=/tmp/grove-scale-images.json`
     → built & pushed grove-operator, grove-initc, grove-install-crds (digest-pinned).
   - `skaffold deploy -p topology-test --default-repo=localhost:5001
     --build-artifacts=/tmp/grove-scale-images.json` → helm release installed.
   - Result: `deployment/grove-operator is ready`. Operator logs confirmed all 3 webhooks
     registered, cert-controller injected TLS certs, controllers started serving on :9443.
   - Webhook configs present: `podcliqueset-defaulting-webhook`,
     `podcliqueset-validating-webhook`, `clustertopology-validating-webhook`.
   - CRDs installed: podcliquesets, podcliques, podcliquescalinggroups, podgangmaps,
     podcliquetemplatespecrevisions, clustertopologybindings (grove.io) + podgangs
     (scheduler.grove.io).

5. **Install KAI:** helm from the public ghcr OCI chart with `kai-values.yaml` (raised
   resource limits to avoid the operator's defaulter throttling/OOMing components).
   KAI images pull directly from ghcr — no local registry needed.
   All 7 components reached Available: kai-operator, admission, binder,
   kai-scheduler-default, pod-grouper, podgroup-controller, queue-controller.

6. **Apply KAI queues:** `kubectl apply -f e2e/yaml/queues.yaml` — succeeded on the FIRST
   attempt (the baseline's webhook race did not recur). Queues default + test created.

7. **Fake node:** applied a KWOK node manifest (`type=kwok` +
   `node_role.e2e.grove.nvidia.com=agent` labels, `kwok.x-k8s.io/node=fake` annotation,
   agent NoSchedule taint, 64 CPU / 512Gi / 110 pods capacity) → **Ready in ~5s**.

8. **End-to-end scale workload:** `kubectl apply -f e2e/yaml/scale-up-tiny.yaml`
   (schedulerName=default-scheduler, `type=kwok` nodeAffinity, agent toleration,
   image registry:5001/nginx:alpine-slim which is never actually pulled), then
   `kubectl patch podcliqueset scale-up-tiny -p '{"spec":{"replicas":5}}'`.

### Result metrics (verified live)

- **PodCliqueSet reconciled correctly:** `scale-up-tiny` shows **REPLICAS=5, AVAILABLE=5**.
- **5 PodCliques**, each MINAVAIL=2, REPLICAS=2, READY=2, SCHEDULED=2.
- **10 expert-worker pods, all Running on `kwok-node-0`** (the fake node), faked by
  kwok-controller in ~8s. Confirmed via `kubectl get pods -o wide`.
- **PodGang CRs (5) stay Pending** — expected: these fixtures bind pods via
  `default-scheduler`, not KAI, so PodGang phase never advances. Grove's availability
  tracking is driven by pod readiness, not PodGang phase, so PCS still reports 5/5
  AVAILABLE. This matches the k3d baseline; the scale tests assert on pod/PCLQ readiness.

### Footprint (verified live: 1 kwok node + full grove/KAI stack + 10 fake pods)

Measured with `docker stats --no-stream`:

| container | image | mem | CPU |
|-----------|-------|-----|-----|
| kwok-grove-scale-control-plane | kindest/node:v1.36.1 | ~1.32-1.34 GiB | ~18-19% |
| kind-registry | registry:2 | ~17-19 MiB | 0% |

- **2 containers, ~1.34-1.36 GiB total.**
- Nodes: 2 (1 real kind control-plane + 1 fake kwok). Pods: 29 total.

### Comparison: k3d baseline vs kwokctl kind

| dimension | k3d baseline | kwokctl kind | delta |
|-----------|--------------|--------------|-------|
| containers | 4 | 2 | -2 (drops serverlb + tools) |
| total mem | ~1.87 GiB | ~1.34 GiB | ~-0.53 GiB (~28% less) |
| main-node mem | ~1.78 GiB (k3s all-in-one) | ~1.32 GiB (kind node) | ~-0.46 GiB |
| idle CPU | 13-44% spiky | ~18% | steadier |

Caveat: the k3d baseline was measured with 100 fake nodes; the kwokctl prototype with 1.
Control-plane memory is dominated by the real components, not fake-node count, so the
comparison is representative — but the kwokctl number should be re-measured at 100+ fake
nodes before treating the delta as final. (Fake nodes/pods live only as apiserver/etcd
objects; expect etcd/apiserver memory to grow with object count on BOTH runtimes.)

## 6. Conclusions

1. **kwokctl `--runtime=kind` is a viable, lighter replacement for k3d** for the grove
   scale tests: proven end-to-end (grove webhooks + certs + KAI + queues + a reconciling
   PodCliqueSet with fake pods Running). ~28% less memory, 2 fewer containers, steadier CPU.

2. **The grove Go tests need NO changes** — they only consume a kubeconfig via
   `setup.SharedCluster()`. The one k3d-specific piece is `StartNodeMonitoring()`
   (restarts dead k3d Docker containers by name); it must become runtime-aware / a no-op
   for the kwok/kind runtime.

3. **Three automation-critical behaviors** the infra-manager must handle for this runtime:
   - **Uncordon** the kind node after creation (else real pods can't schedule).
   - **Pin real components to the real node.** KAI must get ONLY the control-plane
     toleration, NOT the agent (`node_role.e2e.grove.nvidia.com`) toleration. In the
     prototype, giving KAI the agent toleration caused a real KAI pod to schedule onto the
     FAKE kwok node, where kwok "ran" it as a phantom (no real container; `kubectl logs`
     failed dialing the fake node IP 10.0.0.1:10250). Grove already pins correctly.
   - **Provide grove's own image** via a local registry + containerd mirror (KAI's images
     come from public ghcr and need nothing).

4. **kwokctl kind quirks (benign):** the kindnet CNI pod stays in Error/CrashLoop but real
   pods still get IPs and run; kwok Stage CRDs are absent yet the built-in kwok-controller
   still fakes both nodes and pods (so the infra-manager's separate Stage-CR install is
   unnecessary under this runtime).

## 7. Open items / next steps

- Task #3: wire the kwokctl-kind backend into the Python infra-manager (cluster-backend
  selector, registry+mirror setup, uncordon, KAI-toleration fix, skip k3d-registry/
  kubeconfig-merge specifics), add a scale-kwok preset + Makefile targets, make Go
  `StartNodeMonitoring` runtime-aware.
- Re-measure the kwokctl footprint at 100+ fake nodes for an apples-to-apples memory delta.
- Widen the KAI queue-creation retry window in `apply_kai_queues` (baseline bug).
- Decide how grove images are delivered in CI (local registry + mirror, or `kind load`).
