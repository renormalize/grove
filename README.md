
:construction_worker: `This project site is currently under active construction, keep watching for announcements as we approach alpha launch!`

# Grove 🌲 

Grove is a Kubernetes API purpose-built for orchestrating AI workloads in GPU clusters, where a single custom resource allows you to hierarchically compose multiple AI components with flexible gang-scheduling and auto-scaling specfications at multiple levels. Through native support for network topology-aware gang scheduling, multi-dimensional auto-scaling and prescriptive startup ordering, Grove enables developers to define complex AI stacks in a concise, declarative, and framework-agnostic manner.

Grove was originally motivated by the challenges of orchestrating multinode, disaggregated inference systems. It provides a consistent and unified API that allows users to define, configure, and scale prefill, decode, and any other components like routing within a single custom resource. However, it is flexible enough to map naturally to the roles, scaling behaviors, and dependencies of any real-world inference systems, from "traditional" single node aggregated inference to agentic pipelines with multiple models.

## Why Grove?

Modern inference systems are often no longer single-pod workloads. They involve multiple components running across many nodes, often requiring coordination, colocation, custom roles, and precise startup ordering. Inference workloads also need better scheduler coordination to achieve key performance SLAs with features such as network topology-aware gang-scheduling, auto-scaling, rolling upgrades and more. The Grove project was created so that AI developers can define their workload in a declarative manner and influence orchestration level optimizations with easy-to-use, high-level, scheduling policy APIs.


## Core Concepts

The Grove API consists of a user API and a scheduling API. While the user API (`PodGangSet`, `PodClique`, `PodCliqueScalingGroup`) allows users to represent their AI workloads, the scheduling API (`PodGang`) enables scheduler integration to support the network topology-optimized gang-scheduling and auto-scaling requirements of the workload orchestration flows.

| Concept                                                      | Description                                                  |
| ------------------------------------------------------------ | ------------------------------------------------------------ |
| [PodGangSet](https://github.com/nvrohanv/grove/blob/rohanv/doc/update_readme/operator/api/core/v1alpha1/podgangset.go) | The top-level Grove object that defines a group of components managed and colocated together. Also supports autoscaling with topology aware spread of PodGangSet replicas for availability. |
| [PodClique](https://github.com/nvrohanv/grove/blob/rohanv/doc/update_readme/operator/api/core/v1alpha1/podclique.go) | A group of pods representing a specific role (e.g., leader, worker, frontend). Each clique has an independent configuration and supports custom scaling logic. |
| [PodCliqueScalingGroup](https://github.com/nvrohanv/grove/blob/rohanv/doc/update_readme/operator/api/core/v1alpha1/scalinggroup.go) | A set of PodCliques that scale and schedule together. Ideal for tightly coupled roles like prefill leader and worker. |
| [PodGang](scheduler/api/core/v1alpha1/podgang.go)            | The scheduler API that defines a unit of gang-scheduling. A PodGang is a collection of groups of similar pods, where each pod group defines a minimum number of replicas guaranteed for gang-scheduling. |


## Key Capabilities

- ✅ **Declarative Super-Pod Orchestration**  
  Define tightly coupled groups of pods, a.k.a super-pod, with explicit role-based logic. E.g. a group of pods that scale together where one pod is a leader and the rest are workers.

- ✅ **Multi-level Gang Scheduling**  
  Specify gang-scheduling requirements of pods within and across super-pods to prevent resource deadlocks and enable topology-optimized placement.

- ✅ **Multi-Component Coordination and Auto-Scaling**  
  Cleanly represent patterns like prefill/decode disaggregation with independent scaling and resource allocation.

- ✅ **Network Topology-Aware Scheduling**  
  Allows specifying network topology pack and spread constraints within and across super-pods to optimize network performance and service availability.

- ✅ **Custom Startup Dependencies**  
  Prescribe which roles must start before others launch in a declarative specification. Pod startup is decoupled from pod creation or scheduling.

- ✅ **Resource-Aware Rolling Upgrades**  
  Reuse resource reservations across versions during upgrades in order to maintain topology-optimized placement.

- ✅ **Unified Control Plane**  
  Manage inference workers, request routers, and frontend servers together using a single resource definition.

## Example Use Cases

- [Multi-node, Disaggregated Inference for large models (DeepSeek-R1, Llama-4-Maverick)](docs/grove_visualizations/Grove_Multinode_Disagg.png)

- [Single-node, Disaggregated Inference](docs/grove_visualizations/Grove_Single_Node_Disagg.png)

- [Agentic Pipeline of Models](docs/grove_visualizations/Grove_Agentic_Pipeline.png)

- ["Standard" Aggregated Single Node or Single GPU Inference](docs/grove_visualizations/Grove_Single_Node_Agg.png)

## Installation

:construction:

## Community, Discussion, and Support

:construction:

