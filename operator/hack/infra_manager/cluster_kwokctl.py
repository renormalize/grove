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

"""kwokctl --runtime=kind cluster lifecycle.

A single real kindest/node container hosts grove-operator, KAI, and kube-system as normal
in-cluster pods (so webhooks/certs/KAI behave as in production), while the built-in
kwok-controller fakes the scale node/pod fleet. Grove's own images are served from a local
kind-registry container that the node's containerd is configured to mirror.
"""

from __future__ import annotations

import sys
import time

import docker
import sh
from rich.panel import Panel
from tenacity import retry, stop_after_attempt, wait_fixed

from infra_manager import console
from infra_manager.config import ClusterConfig
from infra_manager.constants import (
    CLUSTER_CREATE_RETRY_WAIT_SECONDS,
    KIND_DOCKER_NETWORK,
    KIND_REGISTRY_CONTAINER,
    KIND_REGISTRY_INTERNAL_PORT,
    KWOKCTL_DELETE_SETTLE_SECONDS,
)


def kind_node_name(cluster_name: str) -> str:
    """Return the kind control-plane node/container name for a kwokctl kind cluster.

    kwokctl prefixes its kind cluster with ``kwok-``, so both the Kubernetes node and the
    Docker container are named ``kwok-<cluster_name>-control-plane``.
    """
    return f"kwok-{cluster_name}-control-plane"


def create_cluster_kwokctl(cfg: ClusterConfig) -> None:
    """Create a kwokctl cluster with the kind runtime, retrying on failure.

    Args:
        cfg: Cluster configuration (name, kind_node_image, retry count).

    Raises:
        RetryError: If the cluster cannot be created after all retries.
    """
    console.print(Panel.fit("Creating kwokctl cluster (--runtime=kind)", style="bold blue"))

    @retry(
        stop=stop_after_attempt(cfg.max_retries),
        wait=wait_fixed(CLUSTER_CREATE_RETRY_WAIT_SECONDS),
        reraise=True,
    )
    def _attempt() -> None:
        try:
            sh.kwokctl("delete", "cluster", "--name", cfg.name)
            console.print("[yellow]   Removed existing cluster[/yellow]")
            # kwokctl delete returns before the kind node's docker/containerd resources
            # are fully released. Creating immediately races that teardown and makes the
            # node's containerd briefly report an unsupported config version, which the
            # `kind load docker-image` step (kwokctl uses it to seed the kwok image) then
            # rejects. A short settle lets teardown finish before the next create.
            time.sleep(KWOKCTL_DELETE_SETTLE_SECONDS)
        except sh.ErrorReturnCode:
            console.print("[yellow]   No existing cluster found[/yellow]")

        # Stream kind's progress straight to our stdout/stderr rather than buffering it,
        # so long bring-ups stay visible and never stall on a full capture pipe.
        sh.kwokctl(
            "create",
            "cluster",
            "--name",
            cfg.name,
            "--runtime=kind",
            "--kind-node-image",
            cfg.kind_node_image,
            _out=sys.stdout,
            _err=sys.stderr,
        )

    _attempt()
    console.print("[green]✅ kwokctl cluster created[/green]")


def _ensure_kind_registry(cfg: ClusterConfig) -> None:
    """Ensure a local registry container is running on the kind docker network.

    Grove's operator/initc/install-crds are REAL pods whose images must exist on the node
    (unlike fake workload pods, whose images are never pulled). This registry serves them;
    KAI images come from public ghcr and need nothing.
    """
    console.print("[yellow]ℹ️  Ensuring kind-registry is running...[/yellow]")
    client = docker.from_env()
    try:
        try:
            existing = client.containers.get(KIND_REGISTRY_CONTAINER)
            if existing.status != "running":
                existing.start()
            console.print("[green]   ✓ kind-registry already present[/green]")
            return
        except docker.errors.NotFound:
            pass

        client.containers.run(
            "registry:2",
            name=KIND_REGISTRY_CONTAINER,
            detach=True,
            restart_policy={"Name": "always"},
            network=KIND_DOCKER_NETWORK,
            ports={f"{KIND_REGISTRY_INTERNAL_PORT}/tcp": ("127.0.0.1", cfg.registry_port)},
        )
        console.print(f"[green]   ✓ Started kind-registry on 127.0.0.1:{cfg.registry_port}[/green]")
    finally:
        client.close()


def _configure_registry_mirror(cfg: ClusterConfig) -> None:
    """Point the kind node's containerd at the kind-registry for localhost:<port> pulls.

    The host pushes grove images to localhost:<port>; the node must pull the same content.
    A containerd hosts.toml redirects localhost:<port> to http://kind-registry:5000 so both
    refer to the same registry. Requires a containerd restart (the node survives it).
    """
    console.print("[yellow]ℹ️  Configuring containerd registry mirror on kind node...[/yellow]")
    node = kind_node_name(cfg.name)
    certs_dir = f"/etc/containerd/certs.d/localhost:{cfg.registry_port}"
    hosts_toml = (
        f'[host."http://{KIND_REGISTRY_CONTAINER}:{KIND_REGISTRY_INTERNAL_PORT}"]\n'
        '  capabilities = ["pull", "resolve"]\n'
        "  skip_verify = true\n"
    )
    script = (
        f"set -e\n"
        f"mkdir -p '{certs_dir}'\n"
        f"cat > '{certs_dir}/hosts.toml' <<'EOF'\n"
        f"{hosts_toml}"
        f"EOF\n"
        f"systemctl restart containerd\n"
    )
    sh.docker("exec", node, "bash", "-c", script)
    console.print("[green]   ✓ Registry mirror configured[/green]")


def uncordon_kind_node(cfg: ClusterConfig) -> None:
    """Uncordon the kind control-plane node so real pods (grove, KAI) can schedule.

    The kwokctl kind node comes up cordoned (unschedulable NoSchedule taint); without this,
    no real in-cluster component can be placed.
    """
    console.print("[yellow]ℹ️  Uncordoning kind control-plane node...[/yellow]")
    sh.kubectl("uncordon", kind_node_name(cfg.name))
    console.print("[green]   ✓ Node uncordoned[/green]")


def delete_cluster_kwokctl(cfg: ClusterConfig) -> None:
    """Delete the kwokctl cluster and remove the kind-registry container.

    Args:
        cfg: Cluster configuration with the cluster name.
    """
    console.print(f"[yellow]ℹ️  Deleting kwokctl cluster '{cfg.name}'...[/yellow]")
    try:
        sh.kwokctl("delete", "cluster", "--name", cfg.name)
        console.print(f"[green]✅ Cluster '{cfg.name}' deleted[/green]")
    except sh.ErrorReturnCode:
        console.print(f"[yellow]⚠️  Cluster '{cfg.name}' not found or already deleted[/yellow]")

    client = docker.from_env()
    try:
        registry = client.containers.get(KIND_REGISTRY_CONTAINER)
        registry.remove(force=True)
        console.print("[green]✅ kind-registry removed[/green]")
    except docker.errors.NotFound:
        console.print("[yellow]⚠️  kind-registry not found[/yellow]")
    finally:
        client.close()
