#!/usr/bin/env -S uv run
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

"""
profile-usage.py - Continuous CPU/memory usage collector for scale tests.

Polls resource usage for every relevant pod in the cluster and, separately, the
k3d server container (which hosts the bundled k3s control plane: kube-apiserver,
kube-controller-manager, kube-scheduler, and embedded etcd). Because those
control-plane components run inside a single k3s process rather than as separate
pods, per-pod metrics can't isolate them; the docker-stats sample of the server
container gives their combined footprint.

Requires metrics-server for `kubectl top` (deploy it once with
`hack/deploy-addons.sh --metrics-server`). Docker stats needs Docker access.

Run it in a second terminal alongside `make run-scale-test`:

    ./hack/profile-usage.py --out ./diag --interval 5

Output (appended under --out):
  usage-pods.csv    timestamp,namespace,pod,cpu_millicores,mem_bytes
  usage-server.csv  timestamp,container,cpu_percent,mem_bytes

On Ctrl-C it stops and prints a per-namespace max/mean summary.
"""

from __future__ import annotations

import csv
import re
import signal
import sys
import time
from collections import defaultdict
from datetime import datetime, timezone
from pathlib import Path

import sh
import typer
from rich.console import Console
from rich.table import Table

app = typer.Typer(add_completion=False)
console = Console()

DEFAULT_CLUSTER_NAME = "shared-e2e-test-cluster"
DEFAULT_NAMESPACES = [
    "grove-system",
    "kai-scheduler",
    "kube-system",
    "pyroscope",
]

# CPU quantity suffixes -> millicores multiplier.
_CPU_UNITS = {"n": 1e-6, "u": 1e-3, "m": 1.0, "": 1000.0}
# Memory quantity suffixes -> bytes multiplier (binary + decimal).
_MEM_UNITS = {
    "Ki": 1024,
    "Mi": 1024**2,
    "Gi": 1024**3,
    "Ti": 1024**4,
    "K": 1000,
    "M": 1000**2,
    "G": 1000**3,
    "T": 1000**4,
    "k": 1000,
    "": 1,
}


def parse_cpu_millicores(value: str) -> float:
    """Parse a Kubernetes CPU quantity (e.g. '250m', '2', '500000n') to millicores."""
    match = re.fullmatch(r"(\d+(?:\.\d+)?)([a-zµ]*)", value.strip())
    if not match:
        return 0.0
    num, unit = match.groups()
    unit = "u" if unit in ("µ", "u") else unit
    return float(num) * _CPU_UNITS.get(unit, 1000.0)


def parse_mem_bytes(value: str) -> int:
    """Parse a Kubernetes memory quantity (e.g. '128Mi', '2Gi') to bytes."""
    match = re.fullmatch(r"(\d+(?:\.\d+)?)([A-Za-z]*)", value.strip())
    if not match:
        return 0
    num, unit = match.groups()
    return int(float(num) * _MEM_UNITS.get(unit, 1))


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


def _kubectl_top_pods(namespaces: list[str]) -> list[tuple[str, str, float, int]]:
    """Return (namespace, pod, cpu_millicores, mem_bytes) rows via `kubectl top pods`."""
    rows: list[tuple[str, str, float, int]] = []
    for ns in namespaces:
        try:
            out = sh.kubectl("top", "pods", "-n", ns, "--no-headers")
        except sh.ErrorReturnCode as err:
            console.print(f"[yellow]kubectl top pods -n {ns} failed: {str(err.stderr)[:120]}[/yellow]")
            continue
        for line in str(out).splitlines():
            parts = line.split()
            if len(parts) < 3:
                continue
            pod, cpu, mem = parts[0], parts[1], parts[2]
            rows.append((ns, pod, parse_cpu_millicores(cpu), parse_mem_bytes(mem)))
    return rows


def _docker_stats(container: str) -> tuple[float, int] | None:
    """Return (cpu_percent, mem_bytes) for a docker container, or None on failure."""
    try:
        out = sh.docker(
            "stats",
            container,
            "--no-stream",
            "--format",
            "{{.CPUPerc}};{{.MemUsage}}",
        )
    except sh.ErrorReturnCode as err:
        console.print(f"[yellow]docker stats {container} failed: {str(err.stderr)[:120]}[/yellow]")
        return None
    line = str(out).strip()
    if not line or ";" not in line:
        return None
    cpu_str, mem_str = line.split(";", 1)
    cpu_percent = float(cpu_str.strip().rstrip("%") or 0)
    # MemUsage is like "123.4MiB / 7.5GiB"; take the used side.
    used = mem_str.split("/")[0].strip().replace("iB", "i").replace("B", "")
    return cpu_percent, parse_mem_bytes(used)


def _server_container(cluster_name: str) -> str:
    return f"k3d-{cluster_name}-server-0"


class _Stopper:
    """Flips to True on SIGINT/SIGTERM so the poll loop exits cleanly."""

    def __init__(self) -> None:
        self.stop = False
        signal.signal(signal.SIGINT, self._handle)
        signal.signal(signal.SIGTERM, self._handle)

    def _handle(self, *_: object) -> None:
        self.stop = True


@app.command()
def main(
    out: Path = typer.Option(Path("./diag"), "--out", help="Output directory for CSV files."),
    interval: float = typer.Option(5.0, "--interval", help="Seconds between samples."),
    duration: float = typer.Option(0.0, "--duration", help="Auto-stop after N seconds (0 = run until Ctrl-C)."),
    namespaces: list[str] = typer.Option(DEFAULT_NAMESPACES, "--namespaces", help="Namespaces to sample."),
    cluster_name: str = typer.Option(DEFAULT_CLUSTER_NAME, "--cluster-name", help="k3d cluster name for docker stats."),
) -> None:
    """Poll pod/node/server resource usage until stopped, writing CSV rows per sample."""
    out.mkdir(parents=True, exist_ok=True)
    pods_csv = out / "usage-pods.csv"
    server_csv = out / "usage-server.csv"

    new_pods = not pods_csv.exists()
    new_server = not server_csv.exists()

    server = _server_container(cluster_name)
    stopper = _Stopper()

    # Aggregates for the exit summary: namespace -> list of (cpu_millicores, mem_bytes) totals.
    ns_cpu: dict[str, list[float]] = defaultdict(list)
    ns_mem: dict[str, list[int]] = defaultdict(list)

    console.print(
        f"[bold blue]Sampling usage every {interval}s[/bold blue] "
        f"(namespaces={namespaces}, server={server}, out={out})"
    )
    console.print("[dim]Press Ctrl-C to stop and print a summary.[/dim]")

    start = time.monotonic()
    with (
        pods_csv.open("a", newline="") as pf,
        server_csv.open("a", newline="") as sf,
    ):
        pw, sw = csv.writer(pf), csv.writer(sf)
        if new_pods:
            pw.writerow(["timestamp", "namespace", "pod", "cpu_millicores", "mem_bytes"])
        if new_server:
            sw.writerow(["timestamp", "container", "cpu_percent", "mem_bytes"])

        while not stopper.stop:
            ts = _now()

            sample_cpu: dict[str, float] = defaultdict(float)
            sample_mem: dict[str, int] = defaultdict(int)
            for ns, pod, cpu_m, mem_b in _kubectl_top_pods(namespaces):
                pw.writerow([ts, ns, pod, f"{cpu_m:.1f}", mem_b])
                sample_cpu[ns] += cpu_m
                sample_mem[ns] += mem_b
            for ns in namespaces:
                ns_cpu[ns].append(sample_cpu[ns])
                ns_mem[ns].append(sample_mem[ns])

            stats = _docker_stats(server)
            if stats is not None:
                sw.writerow([ts, server, f"{stats[0]:.2f}", stats[1]])

            pf.flush()
            sf.flush()

            if duration > 0 and (time.monotonic() - start) >= duration:
                break
            # Sleep in short slices so Ctrl-C is responsive.
            slept = 0.0
            while slept < interval and not stopper.stop:
                time.sleep(min(0.25, interval - slept))
                slept += 0.25

    _print_summary(ns_cpu, ns_mem)


def _print_summary(ns_cpu: dict[str, list[float]], ns_mem: dict[str, list[int]]) -> None:
    """Print a per-namespace max/mean summary of total CPU and memory."""
    table = Table(title="Resource usage summary (per-namespace totals)")
    table.add_column("Namespace")
    table.add_column("Samples", justify="right")
    table.add_column("CPU mean (m)", justify="right")
    table.add_column("CPU max (m)", justify="right")
    table.add_column("Mem mean (MiB)", justify="right")
    table.add_column("Mem max (MiB)", justify="right")

    mib = 1024**2
    for ns in sorted(ns_cpu):
        cpus = ns_cpu[ns]
        mems = ns_mem[ns]
        if not cpus:
            continue
        cpu_mean = sum(cpus) / len(cpus)
        mem_mean = sum(mems) / len(mems)
        table.add_row(
            ns,
            str(len(cpus)),
            f"{cpu_mean:.0f}",
            f"{max(cpus):.0f}",
            f"{mem_mean / mib:.0f}",
            f"{max(mems) / mib:.0f}",
        )
    console.print(table)


if __name__ == "__main__":
    try:
        app()
    except KeyboardInterrupt:
        sys.exit(130)
