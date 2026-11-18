#!/usr/bin/env python3
"""Register deployed model workers with modelGateway, or drain them before removal.

`ensure` is safe to rerun: it waits for the local worker, asks the gateway to probe
the public endpoint, creates the physical GPU resource group when missing, registers
the instance, resumes it if paused, and waits until the gateway can dispatch to it.
Any mismatch with an existing group or registration fails instead of editing it.
"""

from __future__ import annotations

import argparse
import json
import os
import stat
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from pathlib import Path

REGISTER_FIELDS = (
    "instance_id",
    "model_key",
    "protocol",
    "endpoint",
    "health_url",
    "resource_group",
    "capacity_units",
    "machine_id",
    "gpu_devices",
)
# Proxies are never used: gateway and worker endpoints are reached directly.
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


class GatewayError(RuntimeError):
    def __init__(self, status, code, message):
        super().__init__(f"HTTP {status} {code}: {message}")
        self.status, self.code = status, code


@dataclass
class Instance:
    instance_id: str
    model_key: str
    protocol: str
    endpoint: str
    health_url: str
    resource_group: str
    machine_id: str
    gpu_devices: list[int]
    capacity_units: int = 1
    group_capacity_units: int = 1
    artifact_digest: str = ""
    local_health_url: str = ""
    input_schema: dict | None = None
    extra: dict = field(default_factory=dict)

    @classmethod
    def parse(cls, data):
        known = {name for name in cls.__dataclass_fields__ if name != "extra"}
        missing = [
            name
            for name in (
                "instance_id",
                "model_key",
                "protocol",
                "endpoint",
                "health_url",
                "resource_group",
                "machine_id",
                "gpu_devices",
            )
            if not data.get(name) and data.get(name) != 0
        ]
        if missing:
            raise ValueError(f"{data.get('instance_id', '<unnamed>')}: missing {', '.join(missing)}")
        item = cls(
            **{key: value for key, value in data.items() if key in known},
            extra={key: value for key, value in data.items() if key not in known},
        )
        item.endpoint = item.endpoint.rstrip("/")
        if not isinstance(item.gpu_devices, list) or not item.gpu_devices or not all(isinstance(x, int) and x >= 0 for x in item.gpu_devices):
            raise ValueError(f"{item.instance_id}: gpu_devices must be a non-empty list of GPU indexes")
        for name in ("capacity_units", "group_capacity_units"):
            if not isinstance(getattr(item, name), int) or getattr(item, name) < 1:
                raise ValueError(f"{item.instance_id}: {name} must be a positive integer")
        return item

    def registration(self):
        body = {name: getattr(self, name) for name in REGISTER_FIELDS}
        if self.input_schema is not None:
            body["input_schema"] = self.input_schema
        if self.artifact_digest:
            body["artifact_digest"] = self.artifact_digest
        return body

    def target(self):
        return {
            "model_key": self.model_key,
            "protocol": self.protocol,
            "endpoint": self.endpoint,
            "health_url": self.health_url,
            "gpu_devices": self.gpu_devices,
        }


def load_manifest(path, only=(), definitions_path=None):
    with open(path) as handle:
        data = json.load(handle)
    defaults = data.get("defaults", {}) if isinstance(data, dict) else {}
    items = data["instances"] if isinstance(data, dict) else data
    definitions = json.loads(Path(definitions_path).read_text())["models"] if definitions_path else {}
    resolved = []
    for item in items:
        item = {**defaults, **item}
        definition = definitions.get(item.get("model_key"))
        if definition:
            if definition["protocol"] != item["protocol"] or ("input_schema" in item and item["input_schema"] != definition["input_schema"]):
                raise ValueError("Manifest and model definition disagree")
            item["input_schema"] = definition["input_schema"]
        if item.get("protocol") in {
            "image_generation",
            "video_generation",
        } and not isinstance(item.get("input_schema"), dict):
            raise ValueError("Generic registration requires an explicit input_schema")
        resolved.append(item)
    instances = [Instance.parse(item) for item in resolved]
    if only:
        unknown = set(only) - {item.instance_id for item in instances}
        if unknown:
            raise ValueError(f"not in manifest: {', '.join(sorted(unknown))}")
        instances = [item for item in instances if item.instance_id in only]
    return instances


def load_credentials(path):
    values = {}
    if path:
        info = os.stat(path)
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077:
            raise SystemExit(f"{path} must be a regular file readable only by its owner (chmod 600)")
        with open(path) as handle:
            for number, line in enumerate(handle, 1):
                line = line.strip()
                if not line or line.startswith("#"):
                    continue
                key, sep, value = line.removeprefix("export ").partition("=")
                if not sep:
                    raise SystemExit(f"{path}:{number}: expected KEY=VALUE")
                values[key.strip()] = value.strip().strip("'\"")
    url = values.get("MODEL_GATEWAY_URL") or os.environ.get("MODEL_GATEWAY_URL", "")
    token = values.get("MODEL_GATEWAY_ADMIN_TOKEN") or os.environ.get("MODEL_GATEWAY_ADMIN_TOKEN", "")
    if not url or not token:
        raise SystemExit("MODEL_GATEWAY_URL and MODEL_GATEWAY_ADMIN_TOKEN are required (credentials file or environment)")
    return url.rstrip("/"), token


class Gateway:
    def __init__(self, base, token, timeout=30):
        self.base, self.token, self.timeout = base, token, timeout

    def call(self, method, path, body=None):
        return self.call_with_headers(method, path, body, {})

    def call_with_headers(self, method, path, body, extra_headers):
        headers = {
            "Authorization": "Bearer " + self.token,
            "Accept": "application/json",
            **extra_headers,
        }
        data = None
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request(self.base + path, data=data, method=method, headers=headers)
        try:
            with OPENER.open(request, timeout=self.timeout) as response:
                raw = response.read()
        except urllib.error.HTTPError as exc:
            raw = exc.read()
            try:
                error = json.loads(raw)["error"]
                code, message = error.get("code", ""), error.get("message", "")
            except (ValueError, KeyError, TypeError, AttributeError):
                code, message = "", raw[:300].decode(errors="replace")
            raise GatewayError(exc.code, code, message) from None
        return json.loads(raw) if raw.strip() else None

    def groups(self):
        return self.call("GET", "/v1/resource-groups") or []

    def instances(self):
        return self.call("GET", "/v1/instances") or []

    def instance(self, instance_id):
        return next((row for row in self.instances() if row.get("id") == instance_id), None)


def log(message):
    print(message, file=sys.stderr, flush=True)


def wait_until(check, deadline, what, interval):
    last = None
    while True:
        try:
            result = check()
            if result:
                return result
        except (OSError, ValueError, GatewayError) as exc:
            last = exc
        if time.monotonic() >= deadline:
            raise TimeoutError(f"timed out waiting for {what}" + (f": {last}" if last else ""))
        time.sleep(interval)


def local_ready(url):
    with OPENER.open(urllib.request.Request(url, headers={"Accept": "application/json"}), timeout=5) as response:
        return json.load(response).get("ready") is True


def ensure_group(gateway, item):
    gpu = item.gpu_devices[0]

    def matching(groups):
        group = next((g for g in groups if g.get("id") == item.resource_group), None)
        if group is not None:
            if group.get("machine_id") != item.machine_id or group.get("gpu_device") != gpu:
                raise SystemExit(
                    f"{item.instance_id}: resource group {item.resource_group} is bound to {group.get('machine_id')}/GPU {group.get('gpu_device')}, not {item.machine_id}/GPU {gpu}"
                )
            return group
        other = next(
            (g for g in groups if g.get("machine_id") == item.machine_id and g.get("gpu_device") == gpu),
            None,
        )
        if other is not None:
            raise SystemExit(f"{item.instance_id}: {item.machine_id}/GPU {gpu} already belongs to resource group {other.get('id')}; use that ID in the manifest")
        return None

    if matching(gateway.groups()):
        return "existing"
    try:
        gateway.call(
            "POST",
            "/v1/resource-groups",
            {
                "id": item.resource_group,
                "machine_id": item.machine_id,
                "gpu_device": gpu,
                "capacity_units": item.group_capacity_units,
            },
        )
        return "created"
    except GatewayError as exc:
        # Another deployment may create the same group concurrently.
        if exc.code == "resource_group_exists" and matching(gateway.groups()):
            return "existing"
        raise


def register(gateway, item, *, initially_draining=False):
    body = item.registration()
    if initially_draining:
        body["initially_draining"] = True
    try:
        gateway.call("POST", "/v1/instances/register", body)
    except GatewayError as exc:
        if exc.status != 409:
            raise
        current = gateway.instance(item.instance_id)
        if current:
            differences = {
                name: (current.get("id" if name == "instance_id" else name), value)
                for name, value in item.registration().items()
                if current.get("id" if name == "instance_id" else name) != value
            }
            detail = f"registered with different settings {differences}; use a new instance_id or edit it in Ops"
        else:
            detail = "the endpoint is still served by another active instance, or this ID was cleared; drain the old instance or use a new instance_id"
        raise SystemExit(f"{item.instance_id}: {exc.code}: {detail}") from None


def stage(gateway, item, timeout, interval):
    """Register paused; unlike ensure, retries never resume an existing instance."""
    existing = gateway.instance(item.instance_id)
    if existing and existing.get("draining") is not True:
        raise ValueError("staging requires an absent or already draining instance")
    deadline = time.monotonic() + timeout
    if item.local_health_url:
        wait_until(
            lambda: local_ready(item.local_health_url),
            deadline,
            "local readiness",
            interval,
        )
    wait_until(
        lambda: gateway.call("POST", "/v1/instances/probe", item.target()).get("ready") is True,
        deadline,
        "Gateway probe",
        interval,
    )
    ensure_group(gateway, item)
    register(gateway, item, initially_draining=True)
    row = gateway.instance(item.instance_id)
    if not row or row.get("draining") is not True:
        # An older server must not silently turn a staged registration into traffic.
        if row:
            gateway.call("POST", f"/v1/instances/{item.instance_id}/drain", {"draining": True})
        raise RuntimeError("Gateway did not honor initially_draining; upgrade Gateway before retrying")
    return row


def ensure(gateway, item, timeout, interval):
    deadline = time.monotonic() + timeout
    if item.local_health_url:
        log(f"{item.instance_id}: waiting for {item.local_health_url}")
        wait_until(
            lambda: local_ready(item.local_health_url),
            deadline,
            f"{item.instance_id} local readiness",
            interval,
        )
    log(f"{item.instance_id}: gateway probe {item.health_url}")
    wait_until(
        lambda: gateway.call("POST", "/v1/instances/probe", item.target()).get("ready") is True,
        deadline,
        f"{item.instance_id} gateway probe",
        interval,
    )
    group = ensure_group(gateway, item)
    log(f"{item.instance_id}: resource group {item.resource_group} {group}")
    register(gateway, item)
    if (gateway.instance(item.instance_id) or {}).get("draining"):
        log(f"{item.instance_id}: resuming dispatch")
        gateway.call("POST", f"/v1/instances/{item.instance_id}/drain", {"draining": False})
    row = wait_until(
        lambda: (lambda r: r if r and r.get("healthy") and r.get("eligible") else None)(gateway.instance(item.instance_id)),
        deadline,
        f"{item.instance_id} to become eligible",
        interval,
    )
    return {
        "instance_id": item.instance_id,
        "model_key": item.model_key,
        "endpoint": item.endpoint,
        "resource_group": item.resource_group,
        "group": group,
        "healthy": row.get("healthy"),
        "eligible": row.get("eligible"),
        "artifact_digest": row.get("artifact_digest", ""),
    }


def drain(gateway, instance_id, timeout, interval, wait):
    if gateway.instance(instance_id) is None:
        return {"instance_id": instance_id, "status": "not_registered"}
    gateway.call("POST", f"/v1/instances/{instance_id}/drain", {"draining": True})
    if wait:
        # Reserved slots include requests that failed externally but may still run on the GPU.
        wait_until(
            lambda: (lambda r: r is not None and not r.get("reserved_requests") and not r.get("reserved_units"))(gateway.instance(instance_id)),
            time.monotonic() + timeout,
            f"{instance_id} reservations to clear",
            interval,
        )
    return {"instance_id": instance_id, "status": "drained" if wait else "draining"}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument(
        "--credentials",
        help="KEY=VALUE file with MODEL_GATEWAY_URL and MODEL_GATEWAY_ADMIN_TOKEN (mode 600)",
    )
    parser.add_argument("--timeout", type=float, default=1800, help="seconds per instance")
    parser.add_argument("--interval", type=float, default=3)
    commands = parser.add_subparsers(dest="command", required=True)
    ensure_cmd = commands.add_parser("ensure", help="register manifest instances once their workers are ready")
    ensure_cmd.add_argument("--model-definitions", help="Schemas emitted by deploy/unified/render.py")
    ensure_cmd.add_argument("manifest")
    ensure_cmd.add_argument("--only", action="append", default=[], help="instance ID to process; repeatable")
    drain_cmd = commands.add_parser("drain", help="pause dispatch before stopping a worker")
    drain_cmd.add_argument("instance_id", nargs="+")
    drain_cmd.add_argument(
        "--no-wait",
        action="store_true",
        help="do not wait for reserved requests to finish",
    )
    status_cmd = commands.add_parser("status", help="show registrations for manifest instances")
    status_cmd.add_argument("--model-definitions", help="Schemas emitted by deploy/unified/render.py")
    status_cmd.add_argument("manifest")
    list_cmd = commands.add_parser("list", help="show all gateway instances whose endpoint contains a substring")
    list_cmd.add_argument("endpoint_filter", nargs="?", default="")
    args = parser.parse_args(argv)

    gateway = Gateway(*load_credentials(args.credentials))
    if args.command == "ensure":
        for item in load_manifest(args.manifest, args.only, args.model_definitions):
            print(
                json.dumps(
                    ensure(gateway, item, args.timeout, args.interval),
                    ensure_ascii=False,
                ),
                flush=True,
            )
    elif args.command == "drain":
        for instance_id in args.instance_id:
            print(
                json.dumps(
                    drain(
                        gateway,
                        instance_id,
                        args.timeout,
                        args.interval,
                        not args.no_wait,
                    )
                ),
                flush=True,
            )
    elif args.command == "list":
        for row in gateway.instances():
            if args.endpoint_filter in (row.get("endpoint") or ""):
                print(
                    json.dumps(
                        {
                            k: row.get(k)
                            for k in (
                                "id",
                                "model_key",
                                "endpoint",
                                "resource_group",
                                "healthy",
                                "eligible",
                                "draining",
                                "reserved_requests",
                                "reserved_units",
                            )
                        }
                    ),
                    flush=True,
                )
    else:
        rows = {row.get("id"): row for row in gateway.instances()}
        for item in load_manifest(args.manifest, definitions_path=args.model_definitions):
            row = rows.get(item.instance_id) or {}
            print(
                json.dumps(
                    {
                        "instance_id": item.instance_id,
                        "registered": bool(row),
                        **{
                            k: row.get(k)
                            for k in (
                                "healthy",
                                "eligible",
                                "draining",
                                "reserved_requests",
                                "endpoint",
                            )
                        },
                    }
                ),
                flush=True,
            )


if __name__ == "__main__":
    try:
        main()
    except (GatewayError, TimeoutError, ValueError, OSError) as exc:
        sys.exit(f"error: {exc}")
