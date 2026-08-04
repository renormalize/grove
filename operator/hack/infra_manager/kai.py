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

"""Kai Scheduler installation and management."""

from __future__ import annotations

from pathlib import Path

import sh
from rich.panel import Panel
from tenacity import retry, stop_after_attempt, wait_fixed

from infra_manager import console
from infra_manager.config import KaiConfig
from infra_manager.constants import (
    CLUSTER_BACKEND_KWOKCTL_KIND,
    E2E_NODE_ROLE_KEY,
    HELM_RELEASE_KAI,
    KAI_QUEUE_MAX_RETRIES,
    KAI_QUEUE_POLL_INTERVAL_SECONDS,
    KAI_SCHEDULER_OCI,
    LABEL_CONTROL_PLANE,
    NS_KAI_SCHEDULER,
    SCRIPT_DIR,
)

KAI_VALUES_FILE = SCRIPT_DIR / "infra_manager" / "kai-values.yaml"


def install_kai_scheduler(cfg: KaiConfig, backend: str = "k3d") -> None:
    """Install Kai Scheduler using Helm.

    Args:
        cfg: Kai Scheduler configuration with the version.
        backend: Cluster backend ("k3d" or "kwokctl-kind"). Under kwokctl-kind, KAI is
            given ONLY the control-plane toleration \u2014 NOT the agent toleration \u2014 so its
            real pods stay on the real kind node instead of landing on (and being phantom-
            "run" by) a fake KWOK node.
    """
    console.print(Panel.fit("Installing Kai Scheduler", style="bold blue"))
    console.print(f"[yellow]Version: {cfg.version}[/yellow]")
    try:
        sh.helm("uninstall", HELM_RELEASE_KAI, "-n", NS_KAI_SCHEDULER)
        console.print("[yellow]   Removed existing Kai Scheduler release[/yellow]")
    except sh.ErrorReturnCode_1:
        console.print("[yellow]   No existing Kai Scheduler release found[/yellow]")

    toleration_args = [
        "--set",
        f"global.tolerations[0].key={LABEL_CONTROL_PLANE}",
        "--set",
        "global.tolerations[0].operator=Exists",
        "--set",
        "global.tolerations[0].effect=NoSchedule",
    ]
    if backend != CLUSTER_BACKEND_KWOKCTL_KIND:
        toleration_args += [
            "--set",
            f"global.tolerations[1].key={E2E_NODE_ROLE_KEY}",
            "--set",
            "global.tolerations[1].operator=Equal",
            "--set",
            "global.tolerations[1].value=agent",
            "--set",
            "global.tolerations[1].effect=NoSchedule",
        ]

    values_args = ["-f", str(KAI_VALUES_FILE)] if KAI_VALUES_FILE.exists() else []

    sh.helm(
        "install",
        HELM_RELEASE_KAI,
        KAI_SCHEDULER_OCI,
        "--version",
        cfg.version,
        "--namespace",
        NS_KAI_SCHEDULER,
        "--create-namespace",
        *toleration_args,
        *values_args,
    )
    console.print("[green]\u2705 Kai Scheduler installed[/green]")


@retry(
    stop=stop_after_attempt(KAI_QUEUE_MAX_RETRIES),
    wait=wait_fixed(KAI_QUEUE_POLL_INTERVAL_SECONDS),
    reraise=True,
)
def apply_kai_queues(queues_file: Path) -> None:
    """Apply Kai queue CRs with retry for webhook readiness.

    Args:
        queues_file: Path to the Kai queues YAML manifest.

    Raises:
        RuntimeError: If the Kai queue webhook is not ready.
    """
    try:
        sh.kubectl("apply", "-f", str(queues_file))
    except sh.ErrorReturnCode as err:
        raise RuntimeError("Kai queue webhook not ready") from err


def uninstall_kai_scheduler() -> None:
    """Uninstall Kai Scheduler via Helm."""
    console.print(Panel.fit("Uninstalling Kai Scheduler", style="bold blue"))
    try:
        sh.helm("uninstall", HELM_RELEASE_KAI, "-n", NS_KAI_SCHEDULER)
        console.print("[green]\u2705 Kai Scheduler uninstalled[/green]")
    except sh.ErrorReturnCode_1:
        console.print("[yellow]No existing Kai Scheduler release found[/yellow]")
