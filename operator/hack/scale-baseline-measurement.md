# Baseline: k3d scale cluster (scale.yaml preset)

Config: worker_nodes=0, kwok.nodes=100, kai+grove+pyroscope+kwok-controller, profiling on.

## Node topology
- total nodes: 101 (1 real k3d server + 100 KWOK fake)

## Docker container footprint (4 containers)
| container | image | idle mem | idle CPU |
|-----------|-------|----------|----------|
| k3d-...-server-0 | rancher/k3s:v1.35.5-k3s1 | ~1.78 GiB | 13-44% (idle, spiky) |
| k3d-...-serverlb | k3d-proxy:5.9.0 | ~23 MiB | 0% |
| k3d-...-tools | k3d-tools:5.9.0 | ~5 MiB | 0% |
| registry | registry:2 | ~62 MiB | 0% |

TOTAL idle mem: ~1.87 GiB across 4 containers.
Idle CPU dominated by k3s server (embedded etcd + apiserver + kcm + scheduler + kwok-controller pod + all in-cluster pods) — 13-44%, notably spiky at idle with 100 kwok nodes.

## In-cluster pods: 18 total

## Notes
- setup failed at KAI queue creation (webhook readiness race, 12 retries too short); queues applied fine manually seconds later. KWOK nodes created manually. Cluster otherwise fully functional.
- The single k3s server container carries ALL load: control plane + kwok-controller + grove + kai + pyroscope. This is the overhead to beat.

## Target for kwokctl --runtime=kind
- Replace 4 k3d containers (server+lb+tools+registry) with 1 kind node container.
- kwokctl runs kwok-controller as part of its stack; kind node runs grove+kai+pyroscope+kube-system.
- Expect: drop serverlb + tools + registry entirely; kind node mem should be comparable to k3s server or less (containerd + kubelet vs k3s all-in-one).

## PROVEN prototype: kwokctl --runtime=kind (cluster "grove-scale")

End-to-end validated by hand. Full stack came up and a scale PodCliqueSet reconciled.

### Footprint (1 kwok node + full grove/KAI stack, 10 fake pods running)
| container | image | mem | CPU |
|-----------|-------|-----|-----|
| kwok-grove-scale-control-plane | kindest/node:v1.36.1 | ~1.32 GiB | ~18% |
| kind-registry | registry:2 | ~17 MiB | 0% |

TOTAL: 2 containers, ~1.34 GiB vs k3d baseline 4 containers / ~1.87 GiB.
Kind node (~1.32 GiB) lighter than k3s all-in-one (~1.78 GiB); drops serverlb+tools.

### Steps that worked (to automate in infra-manager, Task #3)
1. `kwokctl create cluster --name grove-scale --runtime=kind` (kindest/node:v1.36.1).
2. `kubectl uncordon <kind-node>` — kwokctl kind node comes up cordoned; real pods
   (grove, KAI) can't schedule until uncordoned. REQUIRED.
3. Local registry for grove's OWN image (grove-operator/initc/install-crds are real pods):
   - `docker run -d --network kind --name kind-registry -p 127.0.0.1:5001:5000 registry:2`
   - On kind node, add containerd hosts.toml mirroring localhost:5001 -> http://kind-registry:5000
     (dir /etc/containerd/certs.d/localhost:5001/hosts.toml, skip_verify=true), restart containerd.
   - `skaffold build -p topology-test --default-repo=localhost:5001 --file-output=...json`
   - `skaffold deploy -p topology-test --default-repo=localhost:5001 --build-artifacts=...json`
   - KAI images come from public ghcr.io — no local registry needed.
4. KAI install: helm from oci://ghcr.io/kai-scheduler/... with kai-values.yaml.
   CRITICAL: give KAI ONLY the control-plane toleration, NOT the agent
   (node_role.e2e.grove.nvidia.com) toleration. With the agent toleration, real
   KAI pods scheduled onto the fake kwok node and were faked to Running (kubectl
   logs failed: apiserver dialed fake node IP 10.0.0.1:10250). Real components must
   stay on the real kind control-plane node. Grove already pins correctly (no agent tol).
5. Queues applied first try (baseline's webhook race did not recur here).
6. Fake node manifest (type=kwok, agent taint, kwok.x-k8s.io/node=fake) -> Ready in ~5s.
   Scale fixtures use schedulerName=default-scheduler + type=kwok nodeAffinity + agent tol.
7. Applied scale-up-tiny.yaml, scaled to 5 replicas: grove reconciled 5 PCLQs / 10 pods,
   all faked Running on kwok-node-0. PCS shows 5/5 AVAILABLE.
   Note: PodGang CRs stay Pending (pods bind via default-scheduler, not KAI) — expected,
   matches k3d baseline; scale tests check pod/PCLQ readiness not PodGang phase.

### kwokctl kind gotchas
- kindnet CNI pod stays in Error/CrashLoop but real pods still get IPs and run fine.
- kwok Stage CRDs absent — built-in kwok-controller fakes nodes+pods without them.
- Node survives `systemctl restart containerd` inside the kind node.
