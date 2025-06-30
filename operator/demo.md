# demo

## steps

- Bring up the kind cluster in `grove/operator`.

```
make kind-up
```

- Deploy the grove operator.

```
make deploy-local
```

- Switch to KAI-Scheduler and build all components.

```
make build
```

- Package the Helm charts.

```
helm package ./deployments/kai-scheduler -d ./charts
```

- Load the images into the kind cluster (the below is for `fish` shell, please run the equivalent in your shell).

```fish
for img in (docker images --format '{{.Repository}}:{{.Tag}}' | rg "kai-scheduler")
  kind load docker-image $img --name grove-test-cluster
end
```

- Install the charts.

```
helm upgrade -i kai-scheduler -n kai-scheduler --create-namespace ./charts/kai-scheduler-0.0.0.tgz
```

- Create a queue.

```
kubectl apply -f docs/quickstart/queues.yaml
```

- Deploy the `PodGangSet`.

```
kubectl apply -f ./samples/pgs-inorder.yaml
```

## links

- grove: https://github.com/renormalize/grove/tree/demo
- KAI-Scheduler: https://github.com/NVIDIA/KAI-Scheduler/tree/main
