#!/usr/bin/env python3
"""Create model deployments or switch LoRA behind a stable Gateway model_id."""

from __future__ import annotations

import argparse
import contextlib
import fcntl
import hashlib
import json
import os
import re
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request
import uuid
from dataclasses import replace
from pathlib import Path
from urllib.parse import urlencode

from register import (
    OPENER,
    Gateway,
    Instance,
    drain,
    load_credentials,
    stage,
    wait_until,
)

GENERIC = {"image_generation": "images", "video_generation": "videos"}
SUPPORTED = {
    "video_generation",
    "image_generation",
    "product_generation",
    "garment_extraction",
    "vton",
    "body_model",
}
SHA = r"[0-9a-f]{64}"
OPERATIONAL_ENV = {
    "MODEL_GATEWAY_MANAGED",
    "VMIND_IMAGE_MODELS",
    "VMIND_VIDEO_MODELS",
    "VG_DATA_DIR",
    "VG_JOB_STATE_DIR",
    "VG_JOB_OWNER_ID",
    "VMIND_GENERATION_STATE",
    "VMIND_WARMUP_READY_FILE",
    "WARMUP_BASE",
    "WARMUP_MANIFEST",
    "BODYMODEL_STATE",
    "VMIND_STORAGE_REQUIRED_MOUNT",
    "HOME",
    "HF_HOME",
    "CUPY_CACHE_DIR",
    "TMPDIR",
    "LIGHTX2V_CACHE_DIR",
    "TORCHINDUCTOR_CACHE_DIR",
    "CUDA_CACHE_PATH",
    "VMIND_REQUEST_ARCHIVE_DIR",
}


def run(argv, timeout=60):
    result = subprocess.run(argv, capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        # Docker output can contain runtime credentials. Never copy it into deployment records.
        raise RuntimeError(f"{argv[0]} command failed (exit {result.returncode}); inspect locally")
    return result.stdout.strip()


def fingerprint(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def digest(path):
    with open(path, "rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def read_json(url):
    with OPENER.open(url, timeout=5) as response:
        return json.load(response)


def require(condition, message):
    if not condition:
        raise ValueError(message)


class Deployment:
    def __init__(self, path):
        self.path = Path(path).resolve()
        self.data = json.loads(self.path.read_text())
        self.target = self.data["target"]
        self.version = self.data["version"]
        require(
            self.version.get("engine") in {"qwen", "flux", "ltx"} and isinstance(self.version.get("base_model"), str) and self.version["base_model"],
            "declare engine and pinned base_model",
        )
        require(
            isinstance(self.version.get("environment"), dict) and all(isinstance(self.version.get(k), list) for k in ("command", "entrypoint")),
            "declare immutable startup command and inference environment",
        )
        names = [asset["name"] for asset in self.version["assets"]]
        require(
            len(names) == len(set(names)) and set(names) == set(self.target["asset_paths"]),
            "asset names and target paths must match one-to-one",
        )
        self.release = {
            **self.version,
            "compose_sha256": self.target["compose_sha256"],
            "assets": [{**asset, "path": self.target["asset_paths"][asset["name"]]} for asset in self.version["assets"]],
            "cases": self.data["acceptance"],
        }
        self.id = self.data["deployment_id"]
        require(
            re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_.-]{0,79}", self.id),
            "invalid deployment_id",
        )
        self.release_hash = fingerprint(self.version)
        self.identity = fingerprint(self.data)
        self.item = Instance.parse(self.target["instance"])
        require(self.item.protocol in SUPPORTED, "unsupported deployment protocol")
        require(
            re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}", self.data["model_id"]),
            "invalid model_id",
        )
        require(
            self.data["model_id"] == self.item.model_key,
            "model_id must equal Gateway model_key",
        )
        self.validation_model_id = "verify_" + self.identity[:24]
        self.probe = replace(
            self.item,
            instance_id=self.id + "-verify",
            model_key=self.validation_model_id,
        )
        if self.item.protocol in GENERIC:
            for field in ("health_url", "local_health_url"):
                value = getattr(self.item, field)
                require(
                    value.endswith("/v1/" + GENERIC[self.item.protocol] + "/models/" + self.item.model_key + "/health"),
                    "generic model probe URL must name model_id",
                )
                setattr(
                    self.probe,
                    field,
                    value.replace(
                        "/models/" + self.item.model_key + "/health",
                        "/models/" + self.validation_model_id + "/health",
                    ),
                )
        require(self.item.instance_id == self.id, "instance_id must equal deployment_id")
        require(
            len(self.item.gpu_devices) == 1 and self.item.capacity_units == self.item.group_capacity_units == 1,
            "first version supports one GPU and capacity 1",
        )
        require(
            re.fullmatch("sha256-" + SHA, self.item.artifact_digest),
            "explicit Gateway artifact_digest required",
        )
        require(
            self.target.get("adopt_existing") is True or self.item.artifact_digest == "sha256-" + self.release_hash,
            "artifact_digest must identify the complete version",
        )
        require(self.item.local_health_url, "local_health_url required")
        if self.item.protocol in GENERIC:
            require(
                isinstance(self.item.input_schema, dict),
                "generic input_schema required",
            )
        require(
            re.fullmatch(r"[^\s]+@sha256:" + SHA, self.release["image"]),
            "registry image digest required",
        )
        require(
            self.release["assets"] and self.release["cases"],
            "assets and acceptance cases required",
        )
        require(
            {"weights", "lora"} & {a["role"] for a in self.release["assets"]},
            "pin model weights",
        )
        require(
            all(re.fullmatch(SHA, a["sha256"]) for a in self.release["assets"]),
            "invalid asset digest",
        )
        require(
            all(a.get("source", "").startswith(("s3://", "hf://", "https://", "git://")) for a in self.release["assets"]),
            "each asset needs a cloud source (without credentials) as well as a local cache path",
        )
        for case in self.release["cases"]:
            require(re.fullmatch(SHA, case["sha256"]), "invalid case digest")
            require(
                re.fullmatch(SHA, case["expected_generation"]["model_version.sha256"]),
                "expected weight SHA required",
            )
            require(type(case.get("require_pe")) is bool, "explicit require_pe required")
            require(
                isinstance(case.get("expected_size"), list) and len(case["expected_size"]) == 2 and all(type(x) is int and x > 0 for x in case["expected_size"]),
                "expected output dimensions required",
            )
        if self.item.protocol == "video_generation":
            require(self.version["engine"] == "ltx" and self.version.get("binding") == {"model": "ltx"}, "video deployment supports the LTX worker only")
            for case in self.release["cases"]:
                require(type(case.get("expected_frames")) is int and case["expected_frames"] > 0, "expected video frame count required")
                require(type(case.get("expected_fps")) in (int, float) and case["expected_fps"] > 0, "expected video frame rate required")
                roles = {a["role"] for a in self.release["assets"]}
                require("weights" in roles, "pin video base weights")
                if "lora" in roles:
                    require(re.fullmatch(SHA, case["expected_generation"].get("model_version.lora.sha256", "")), "expected video LoRA SHA required")
        self.compose_path = (self.path.parent / self.target["compose"]).resolve()
        require(
            Path(self.target["compose"]).name == self.target["compose"] and self.target["compose"] not in {"manifest.json", ".", ".."},
            "compose must be a filename next to the manifest",
        )
        self.service = self.target["service"]
        self.project = self.target.get("project", self.id)
        self.state_root = Path(self.target["state_root"])
        require(self.state_root.is_absolute(), "state_root must be absolute")
        require(
            self.target["hostname"] and self.target["gpu_uuid"].startswith("GPU-"),
            "pin target hostname and GPU UUID",
        )

    def validator(self):
        name = self.version.get("output_validator")
        asset = next(
            (a for a in self.release["assets"] if a["name"] == name and a["role"] == "validator"),
            None,
        )
        require(asset is not None, "declare a pinned model-specific output_validator asset")
        path = Path(asset["path"])
        require(
            path.is_absolute() and digest(path) == asset["sha256"],
            "output validator digest mismatch",
        )
        return path

    def compose(self, *args):
        # Keep the content-derived key out of the hashed Compose source (no circular hash).
        return [
            "env",
            "MODEL_ID=" + self.item.model_key,
            "VALIDATION_MODEL_ID=" + self.validation_model_id,
            "docker",
            "compose",
            "-p",
            self.project,
            "-f",
            str(self.compose_path),
            *args,
        ]

    def case_path(self, case):
        return Path(case["path"])

    def request_id(self, index):
        return str(uuid.uuid5(uuid.NAMESPACE_URL, self.identity + ":" + str(index)))

    def check_files(self):
        require(
            digest(self.compose_path) == self.release["compose_sha256"],
            "Compose changed; declare a new deployment",
        )
        for entry in self.release["assets"] + self.release["cases"]:
            path = Path(entry["path"])
            require(path.is_absolute(), "runtime asset/case paths must be absolute")
            require(
                digest(path) == entry["sha256"],
                "asset/case digest mismatch: " + str(path),
            )

    def config(self):
        config = json.loads(run(self.compose("config", "--format", "json")))
        require(
            set(config["services"]) == {self.service},
            "use a separate, single-service Compose file",
        )
        service = config["services"][self.service]
        for key in ("command", "entrypoint"):
            require(
                service.get(key, []) == self.version[key],
                "model startup command changed outside version definition",
            )
        require(
            re.fullmatch(r"[0-9]+:[0-9]+", service.get("user", "")),
            "declare numeric container user UID:GID",
        )
        require(
            service["image"] == self.release["image"],
            "Compose image differs from version",
        )
        require(
            not any(
                service.get(k)
                for k in (
                    "build",
                    "depends_on",
                    "network_mode",
                    "volumes_from",
                    "devices",
                    "gpus",
                )
            ),
            "use explicit reserved GPU; no build, dependencies or shared network namespace",
        )
        require(
            not service.get("container_name") or service["container_name"] == self.id,
            "container_name must equal deployment_id",
        )
        devices = service.get("deploy", {}).get("resources", {}).get("reservations", {}).get("devices", [])
        require(
            len(devices) == 1 and devices[0].get("device_ids") == [str(self.item.gpu_devices[0])] and devices[0].get("capabilities") == ["gpu"],
            "pin exactly the declared physical GPU",
        )
        mounts = service.get("volumes", [])
        require(
            mounts and all(m.get("type") == "bind" for m in mounts),
            "only explicit bind mounts supported",
        )
        require(
            any(not m.get("read_only") for m in mounts),
            "persistent Worker state mount required",
        )
        for mount in mounts:
            path = Path(mount["source"]).resolve()
            if not mount.get("read_only"):
                require(
                    path.is_relative_to(self.state_root.resolve()),
                    "writable mounts must be inside deployment state_root",
                )
        for asset in self.release["assets"]:
            require(
                any(m.get("read_only") and Path(asset["path"]).resolve().is_relative_to(Path(m["source"]).resolve()) for m in mounts),
                "each version asset must be mounted read-only",
            )
        ports = service.get("ports", [])
        require(
            ports and all(str(p.get("published", "")).isdigit() and p.get("protocol", "tcp") == "tcp" for p in ports),
            "explicit TCP host ports required",
        )
        require(len({p["published"] for p in ports}) == len(ports), "duplicate host ports")
        env = service.get("environment", {})
        if self.item.protocol in GENERIC:
            binding_env = "VMIND_VIDEO_MODELS" if self.item.protocol == "video_generation" else "VMIND_IMAGE_MODELS"
            bindings = json.loads(env.get(binding_env, "{}"))
            expected = {self.item.model_key} if self.target.get("adopt_existing") else {self.item.model_key, self.validation_model_id}
            require(
                set(bindings) == expected,
                "Worker bindings do not match the declared deployment",
            )
            require(
                all(binding == self.version["binding"] for binding in bindings.values()),
                "validation binding must use the same version/profile",
            )
        if self.item.protocol == "video_generation":
            # Single physical GPU is exposed as cuda:0. Never accept a shared production topology.
            topology_path = env.get("VG_INSTANCES", "")
            topology_assets = []
            for asset in self.release["assets"]:
                if asset["role"] != "config":
                    continue
                for mount in mounts:
                    source = Path(mount["source"]).resolve()
                    asset_path = Path(asset["path"]).resolve()
                    if mount.get("read_only") and asset_path.is_relative_to(source):
                        if str(Path(mount["target"]) / asset_path.relative_to(source)) == topology_path:
                            topology_assets.append(asset)
            require(len(topology_assets) == 1, "pin VG_INSTANCES as a read-only config asset")
            topology = json.loads(Path(topology_assets[0]["path"]).read_text())
            instances = topology.get("instances") or []
            require(
                len(instances) == 1 and instances[0].get("model") == "ltx"
                and instances[0].get("gpus") == [0] and topology.get("gemma_gpu") is None,
                "video deployment requires one LTX worker on container GPU 0",
            )
            require(not env.get("VG_DATABASE_URL"), "video deployment uses its persistent local execution ledger")
        inference = {
            key: str(value)
            for key, value in env.items()
            if key not in OPERATIONAL_ENV
            and not re.search(
                r"(?:^|_)(?:TOKEN|SECRET|PASSWORD|API_KEY|ACCESS_KEY|CREDENTIALS?)(?:$|_)",
                key,
                re.I,
            )
        }
        require(
            inference == {key: str(value) for key, value in self.version["environment"].items()},
            "inference environment differs from immutable version",
        )
        state_names = ("VG_DATA_DIR", "VG_JOB_STATE_DIR") if self.item.protocol == "video_generation" else ("VMIND_GENERATION_STATE", "VMIND_WARMUP_READY_FILE")
        for name in state_names:
            require(
                env.get(name) and any(not m.get("read_only") and Path(env[name]).is_relative_to(m["target"]) for m in mounts),
                name + " must use persistent deployment state",
            )
        require(
            str(env.get("MODEL_GATEWAY_MANAGED")) == "1",
            "worker must be Gateway managed",
        )
        return config, service


def registration_matches(row, item):
    return all(row.get("id" if key == "instance_id" else key) == value for key, value in item.registration().items())


def check_gateway(gateway, deployment, previous=None):
    item = deployment.item
    allowed = {deployment.probe.instance_id: deployment.probe}
    if previous:
        allowed.update(
            {
                previous.item.instance_id: previous.item,
                previous.probe.instance_id: previous.probe,
            }
        )
    rows = gateway.instances()
    for row in rows:
        if row["id"] == item.instance_id:
            require(
                registration_matches(row, item),
                "existing instance differs from pinned registration",
            )
        elif row["id"] in allowed:
            require(
                registration_matches(row, allowed[row["id"]]),
                "related registration drift",
            )
        else:
            retired = row.get("draining") is True and row.get("healthy") is False and row.get("reserved_requests") == 0 and row.get("reserved_units") == 0
            if retired:
                continue  # immutable, paused history; memory and container checks still apply
            require(
                row.get("model_key") != item.model_key,
                "model_id already has another instance",
            )
            require(
                row.get("endpoint", "").rstrip("/") != item.endpoint,
                "endpoint is shared with another registration",
            )
            same_gpu = row.get("resource_group") == item.resource_group or (row.get("machine_id") == item.machine_id and item.gpu_devices[0] in row.get("gpu_devices", []))
            retired_eval = (
                row["id"].startswith("eval-")
                and row.get("draining") is True
                and row.get("healthy") is False
                and row.get("reserved_requests") == 0
                and row.get("reserved_units") == 0
            )
            require(
                not same_gpu or retired_eval,
                "first version requires an unused GPU resource group",
            )
    for group in gateway.groups():
        same_gpu = group.get("machine_id") == item.machine_id and group.get("gpu_device") == item.gpu_devices[0]
        if same_gpu or group["id"] == item.resource_group:
            require(
                same_gpu and group["id"] == item.resource_group and group["capacity_units"] == 1,
                "reuse the physical GPU group without changing its capacity",
            )
    return next((r for r in rows if r["id"] == item.instance_id), None)


class Runner:
    def __init__(self, deployment, gateway, timeout=1800, api_token="", audit=None, previous=None):
        self.d, self.gateway, self.timeout, self.api_token, self.audit = (
            deployment,
            gateway,
            timeout,
            api_token,
            audit,
        )
        self.previous = previous

    def gateway_state(self):
        return check_gateway(self.gateway, self.d, self.previous)

    def containers(self):
        ids = run(["docker", "ps", "-aq"]).split()
        return json.loads(run(["docker", "inspect", *ids])) if ids else []

    def owned(self, containers):
        owned = [c for c in containers if c["Config"].get("Labels", {}).get("com.docker.compose.project") == self.d.project]
        require(len(owned) <= 1, "deployment project contains unexpected containers")
        if owned:
            labels = owned[0]["Config"]["Labels"]
            require(
                labels.get("com.docker.compose.service") == self.d.service,
                "unexpected deployment service",
            )
            expected = run(self.d.compose("config", "--hash", self.d.service)).split()[-1]
            require(
                labels.get("com.docker.compose.config-hash") == expected,
                "existing container configuration drift",
            )
            image = json.loads(run(["docker", "image", "inspect", self.d.release["image"]]))[0]["Id"]
            require(owned[0]["Image"] == image, "existing container image drift")
        return owned[0] if owned else None

    def preflight(self):
        d = self.d
        require(
            socket.gethostname() == d.target["hostname"],
            "run on the declared target host",
        )
        d.check_files()
        config, service = d.config()
        row = self.gateway_state()
        run(["docker", "image", "inspect", d.release["image"]])
        containers = self.containers()
        owned = self.owned(containers)
        predecessor = Runner(self.previous, self.gateway).owned(containers) if self.previous else None
        same_gpu = self.previous and self.previous.target["gpu_uuid"] == d.target["gpu_uuid"]
        if row:
            require(
                owned and owned["State"]["Running"],
                "registered worker is absent/stopped; do not recreate an instance with execution history",
            )
        for container in containers:
            if owned and container["Id"] == owned["Id"]:
                continue
            for mount in container.get("Mounts", []):
                source = Path(mount["Source"]).resolve()
                root = d.state_root.resolve()
                require(
                    not (source.is_relative_to(root) or root.is_relative_to(source)),
                    "state_root overlaps another container mount",
                )
            if container["State"]["Running"]:
                if predecessor and container["Id"] == predecessor["Id"]:
                    continue
                requests = container.get("HostConfig", {}).get("DeviceRequests") or []
                for request in requests:
                    ids = request.get("DeviceIDs") or []
                    require(
                        request.get("Count") != -1 and str(d.item.gpu_devices[0]) not in ids and d.target["gpu_uuid"] not in ids,
                        "GPU allocated to another container",
                    )
        gpu = run(
            [
                "nvidia-smi",
                "-i",
                str(d.item.gpu_devices[0]),
                "--query-gpu=uuid,memory.used",
                "--format=csv,noheader,nounits",
            ]
        ).split(",")
        require(gpu[0].strip() == d.target["gpu_uuid"], "physical GPU identity mismatch")
        if not owned:
            require(
                not d.state_root.exists(),
                "fresh deployment needs a new state_root; never reuse old idempotency state",
            )
            uid, gid = map(int, service["user"].split(":"))
            require(
                os.geteuid() == 0 or (os.geteuid(), os.getegid()) == (uid, gid),
                "run as container UID:GID or root to create owned state directories",
            )
            require(
                (same_gpu and predecessor and predecessor["State"]["Running"]) or int(gpu[1].strip()) <= 256,
                "GPU not idle outside the declared predecessor",
            )
            old_ports = {str(p["published"]) for p in self.previous.config()[1]["ports"]} if predecessor and predecessor["State"]["Running"] else set()
            for port in service["ports"]:
                if str(port["published"]) in old_ports:
                    require(same_gpu, "spare-GPU candidate needs independent ports")
                    continue
                with socket.socket() as sock:
                    sock.bind((port.get("host_ip") or "0.0.0.0", int(port["published"])))
        return {
            "deployment_id": d.id,
            "version_id": d.release_hash,
            "deployment_sha256": d.identity,
            "compose_runtime_sha256": fingerprint(config),
            "model_id": d.item.model_key,
            "instance_id": d.item.instance_id,
            "gateway": self.gateway.base,
            "request_ids": [d.request_id(i) for i in range(len(d.release["cases"]))],
        }

    def status(self):
        require(
            socket.gethostname() == self.d.target["hostname"],
            "run on the declared target host",
        )
        require(
            digest(self.d.compose_path) == self.d.release["compose_sha256"],
            "Compose changed",
        )
        row = self.gateway_state()
        owned = self.owned(self.containers())
        worker = read_json(self.d.target["idle_url"]) if owned and owned["State"]["Running"] else None
        return {
            "deployment_id": self.d.id,
            "model_id": self.d.item.model_key,
            "registered": bool(row),
            "container_running": bool(owned and owned["State"]["Running"]),
            "gateway": {
                k: (row or {}).get(k)
                for k in (
                    "healthy",
                    "eligible",
                    "draining",
                    "reserved_requests",
                    "reserved_units",
                )
            },
            "worker": {k: (worker or {}).get(k) for k in ("ready", "active_tasks", "pending_tasks")},
            "version_id": self.d.release_hash,
            "worker_reported_weights": {
                "body_model": (worker or {}).get("checkpoint_sha256"),
                "vton": ((worker or {}).get("vton_checkpoint") or {}).get("sha256"),
            },
        }

    def prepare_state(self):
        _, service = self.d.config()
        uid, gid = map(int, service["user"].split(":"))
        # Called only after a committed deploy_intent; never chown a shared directory.
        root = self.d.state_root
        root.mkdir(mode=0o750)  # parent must be provisioned; no silent reuse
        os.chown(root, uid, gid)
        for mount in service["volumes"]:
            if not mount.get("read_only"):
                path = Path(mount["source"])
                path.mkdir(parents=True, exist_ok=True, mode=0o750)
                for directory in (path, *path.parents):
                    if not directory.is_relative_to(root):
                        break
                    os.chown(directory, uid, gid)

    def paused(self):
        self.gateway.call("POST", "/v1/instances/" + self.d.id + "/drain", {"draining": True})

    def accept(self, item=None):
        api = Gateway(self.gateway.base, self.api_token, timeout=self.timeout)
        item = item or self.d.item
        for index, case in enumerate(self.d.release["cases"]):
            request_id = self.d.request_id(index)
            payload = json.loads(self.d.case_path(case).read_text())
            if item.protocol in GENERIC:
                require("model" not in payload, "case contains input fields only")
                path, body = (
                    "/v1/videos" if item.protocol == "video_generation" else "/v1/images/generations",
                    {"model": item.model_key, **payload},
                )
            else:
                path, body = (
                    "/v1/gateway/requests/" + item.model_key,
                    {"input": payload},
                )
            self.audit.record("acceptance_intent", request_id=request_id)
            api.call_with_headers("POST", path, body, {"Idempotency-Key": request_id})
            detail = self.gateway.call("GET", "/v1/admin/requests/" + request_id)
            if item.protocol == "video_generation":
                deadline = time.monotonic() + self.timeout
                while detail.get("state") in {"queued", "processing"}:
                    require(time.monotonic() < deadline, "video acceptance timed out; execution remains unknown")
                    time.sleep(min(2, max(0, deadline - time.monotonic())))
                    detail = self.gateway.call("GET", "/v1/admin/requests/" + request_id)
            require(
                detail.get("state") == "succeeded" and detail.get("instance_id") == item.instance_id,
                "acceptance did not succeed on the selected instance",
            )
            generation = (detail.get("result") or {}).get("generation") or {}
            for dotted, expected in case["expected_generation"].items():
                value = generation
                for key in dotted.split("."):
                    value = value.get(key) if isinstance(value, dict) else None
                require(value == expected, "generation receipt mismatch: " + dotted)
            require(
                not case["require_pe"] or generation.get("pe_status") == "applied",
                "PE fell back or did not run",
            )
            url = self.gateway.base + "/v1/admin/requests/" + request_id + "/media?kind=output&index=0"
            request = urllib.request.Request(url, headers={"Authorization": "Bearer " + self.api_token})
            with tempfile.TemporaryDirectory(prefix="vmind-accept-") as directory:
                media = Path(directory) / "output"
                settings = Path(directory) / "case.json"
                # Stream bounded artifacts; video output must not consume unbounded RAM.
                limit = (512 if item.protocol == "video_generation" else 64) * 1024 * 1024
                size = 0
                output_hash = hashlib.sha256()
                with OPENER.open(request, timeout=60) as response, media.open("wb") as stream:
                    while chunk := response.read(1024 * 1024):
                        size += len(chunk)
                        require(size <= limit, "acceptance output too large")
                        stream.write(chunk)
                        output_hash.update(chunk)
                settings.write_text(json.dumps(case))
                validation = json.loads(
                    run(
                        [
                            sys.executable,
                            str(self.d.validator()),
                            "--media",
                            str(media),
                            "--case",
                            str(settings),
                        ],
                        timeout=60,
                    )
                )
                require(
                    isinstance(validation, dict),
                    "model output validator must return a JSON object",
                )
            self.audit.record(
                "acceptance_passed",
                request_id=request_id,
                instance_id=item.instance_id,
                output_sha256=output_hash.hexdigest(),
                validation=validation,
                weight_sha256=generation["model_version"]["sha256"],
                pe_status=generation.get("pe_status"),
            )

    def start(self, restoring=False):
        self.audit.record("start_intent", restoring=restoring)
        owned = self.owned(self.containers())
        if not owned:
            require(
                not restoring,
                "rollback container missing; do not reconstruct execution history",
            )
            self.prepare_state()
        elif owned["State"]["Running"]:
            return
        else:
            require(restoring, "stopped candidate requires explicit recovery")
        run(
            self.d.compose(
                "up",
                "-d",
                "--no-build",
                "--no-recreate",
                "--pull",
                "never",
                "--wait",
                "--wait-timeout",
                str(self.timeout),
            ),
            timeout=self.timeout + 30,
        )
        self.audit.record("started")

    def validate(self):
        self.validation_supported()
        # Same worker/profile, isolated model key: never redirect normal traffic to an untested version.
        stage(self.gateway, self.d.probe, self.timeout, 2)
        self.audit.record("validation_intent", model_id=self.d.validation_model_id)
        try:
            self.gateway.call(
                "POST",
                "/v1/instances/" + self.d.probe.instance_id + "/drain",
                {"draining": False},
            )
            wait_until(
                lambda: (self.gateway.instance(self.d.probe.instance_id) or {}).get("eligible"),
                time.monotonic() + self.timeout,
                "validation instance readiness",
                2,
            )
            self.accept(self.d.probe)
        finally:
            # Unknown execution is retained for quiescence/recovery, never force-killed.
            self.gateway.call(
                "POST",
                "/v1/instances/" + self.d.probe.instance_id + "/drain",
                {"draining": True},
            )
        drain(self.gateway, self.d.probe.instance_id, self.timeout, 2, True)
        self.audit.record("validated")

    def validation_supported(self):
        require(self.api_token, "MODEL_GATEWAY_API_TOKEN required for acceptance")
        require(
            not self.d.target.get("adopt_existing"),
            "a replacement must support isolated validation",
        )
        self.d.validator()

        self.d.config()

    def activate(self):
        self.gateway_state()
        if self.previous:
            old = self.gateway.instance(self.previous.item.instance_id)
            require(
                not old or old.get("draining") is True,
                "old instance must be paused before activating its replacement",
            )
        stage(self.gateway, self.d.item, self.timeout, 2)
        self.audit.record("activation_intent")
        try:
            self.gateway.call("POST", "/v1/instances/" + self.d.id + "/drain", {"draining": False})
            wait_until(
                lambda: (self.gateway.instance(self.d.id) or {}).get("eligible"),
                time.monotonic() + self.timeout,
                "model readiness",
                2,
            )
            self.audit.record("active")
        except BaseException:
            self.paused()
            raise
        return {
            "phase": "active",
            "model_id": self.d.item.model_key,
            "version_id": self.d.release_hash,
        }

    def deploy(self, activate=False, prepared_plan=None):
        plan = self.preflight() if prepared_plan is None else prepared_plan
        require(self.audit is not None, "database audit required")
        events = self.audit.state["events"]
        require(
            not any(e["phase"] in {"stop_intent", "stopped"} for e in events),
            "stopped deployments cannot be reused; create a new instance_id",
        )
        previous = next((e for e in events if e["phase"] == "deploy_intent"), None)
        if previous:
            require(
                previous["plan"] == plan,
                "runtime configuration or Gateway drift since deployment",
            )
        row = self.gateway.instance(self.d.id)
        require(not row or row.get("draining"), "instance already active; use status")
        if activate:
            require(self.api_token, "MODEL_GATEWAY_API_TOKEN required for acceptance")
        self.audit.record("deploy_intent", plan=plan)
        self.start()
        if activate:
            self.validate()
            return self.activate()
        stage(self.gateway, self.d.item, self.timeout, 2)
        self.audit.record("staged")
        return {"phase": "staged", "draining": True}

    def quiesce(self, allow_queued=False):
        self.gateway_state()
        for item in (self.d.item, self.d.probe):
            if self.gateway.instance(item.instance_id):
                self.gateway.call(
                    "POST",
                    "/v1/instances/" + item.instance_id + "/drain",
                    {"draining": True},
                )
        for item in (self.d.item, self.d.probe):
            drain(self.gateway, item.instance_id, self.timeout, 2, True)
            if self.gateway.instance(item.instance_id):
                for state in ("queued", "processing"):
                    query = {"model": item.model_key, "state": state, "limit": 1}
                    if item == self.d.item and allow_queued:
                        query["instance_id"] = item.instance_id  # only unassigned work may move to a new version
                    pending = self.gateway.call("GET", "/v1/admin/requests?" + urlencode(query))
                    require(
                        pending.get("total") == 0,
                        "pending requests; stop callers or resolve queued work",
                    )
        owned = self.owned(self.containers())
        exposed = any(self.gateway.instance(item.instance_id) for item in (self.d.item, self.d.probe))
        if owned and owned["State"]["Running"] and exposed:

            def idle():
                data = read_json(self.d.target["idle_url"])
                if self.d.item.protocol == "video_generation":
                    readiness = data.get("readiness") or {}
                    workers = readiness.get("workers") or []
                    instances = data.get("instances") or []
                    return (
                        len(workers) == len(instances) == 1
                        and instances[0].get("gpus") == [0]
                        and instances[0].get("model") == self.d.version["binding"].get("model")
                        and workers[0].get("name") == instances[0].get("name")
                        and readiness.get("admitted") == 0
                        and data.get("queued_total") == 0
                        and workers[0].get("busy") is False
                    )
                return "active_tasks" in data and "pending_tasks" in data and not data["active_tasks"] and not data["pending_tasks"] and not data.get("current_task")

            wait_until(
                idle,
                time.monotonic() + self.timeout,
                "Worker execution to become idle",
                2,
            )
        self.gateway_state()
        for item in (self.d.item, self.d.probe):
            row = self.gateway.instance(item.instance_id)
            require(
                not row or (row.get("draining") is True and row.get("reserved_requests") == 0 and row.get("reserved_units") == 0),
                "Gateway reservations/drain changed",
            )
        return owned

    def stop(self, allow_queued=False):
        require(self.audit is not None, "database audit required")
        require(
            socket.gethostname() == self.d.target["hostname"],
            "run on the declared target host",
        )
        require(
            digest(self.d.compose_path) == self.d.release["compose_sha256"],
            "Compose changed",
        )
        self.d.config()
        self.audit.record("stop_intent")
        owned = self.quiesce(allow_queued=allow_queued)
        if owned and owned["State"]["Running"]:
            run(self.d.compose("stop", "--timeout", "60", self.d.service), timeout=90)
        self.audit.record("stopped")
        return {
            "phase": "stopped",
            "state_preserved": True,
            "registration_preserved": True,
        }


class Switch:
    """Single-host, single-worker replacement using existing Gateway APIs only."""

    def __init__(self, old, new):
        self.old, self.new = old, new
        old.previous, new.previous = new.d, old.d

    def plan(self):
        a, b = self.old.d, self.new.d
        require(
            a.id != b.id and a.project != b.project,
            "replacement needs a new deployment_id and Compose project",
        )
        require(
            a.item.model_key == b.item.model_key,
            "switch-lora must preserve model_id; use deploy for a new model_id",
        )
        require(
            a.item.protocol == b.item.protocol and a.item.input_schema == b.item.input_schema,
            "stable model_id requires the same protocol and schema",
        )
        require(
            a.target["hostname"] == b.target["hostname"] == socket.gethostname(),
            "switch must run on the declared host; cross-host orchestration is not supported",
        )
        require(
            a.version["engine"] == b.version["engine"] and a.version["base_model"] == b.version["base_model"],
            "LoRA switch requires a compatible engine/base model",
        )

        def base(d):
            return {x["name"]: x["sha256"] for x in d.version["assets"] if x["role"] == "weights"}

        def lora(d):
            return {x["name"]: x["sha256"] for x in d.version["assets"] if x["role"] == "lora"}

        require(
            base(a) == base(b) and lora(a) and lora(b) and lora(a) != lora(b),
            "switch-lora must change LoRA while preserving base weights",
        )
        require(
            not (a.state_root.is_relative_to(b.state_root) or b.state_root.is_relative_to(a.state_root)),
            "old and candidate state must be independent",
        )
        if a.target["gpu_uuid"] == b.target["gpu_uuid"]:
            require(
                a.item.resource_group == b.item.resource_group,
                "same GPU must retain its resource_group",
            )
        a.check_files()
        a.config()
        self.old.gateway_state()
        original = self.old.owned(self.old.containers())
        require(
            original and original["State"]["Running"],
            "predecessor must be running; use rollback for an interrupted switch",
        )
        row = self.old.gateway.instance(a.id)
        require(
            row and row.get("eligible") and not row.get("draining"),
            "predecessor must be eligible before a new switch",
        )
        self.new.preflight()
        return {
            "model_id": a.item.model_key,
            "from_deployment": a.id,
            "to_deployment": b.id,
            "from_version": a.release_hash,
            "to_version": b.release_hash,
            "strategy": "reuse_gpu" if a.target["gpu_uuid"] == b.target["gpu_uuid"] else "spare_gpu",
        }

    def rollback(self):
        self.new.audit.record("rollback_intent", predecessor=self.old.d.id)
        # If this cannot prove the candidate idle, leave it paused and do not restart an old GPU owner.
        self.new.stop(allow_queued=True)
        self.new.audit.record("rollback_restoring")
        self.old.d.check_files()
        self.old.d.config()
        current = self.old.gateway.instance(self.old.d.id)
        if not current or current.get("draining"):
            self.old.start(restoring=True)
            self.old.activate()
        else:
            require(
                current.get("eligible"),
                "predecessor is active but unhealthy; inspect before declaring rollback complete",
            )
            self.old.audit.record("active")
        self.new.audit.record(
            "rolled_back",
            restored_model_id=self.old.d.item.model_key,
            restored_version=self.old.d.release_hash,
        )
        return {
            "phase": "rolled_back",
            "model_id": self.old.d.item.model_key,
            "version_id": self.old.d.release_hash,
        }

    def apply(self, plan):
        require(self.new.api_token, "MODEL_GATEWAY_API_TOKEN required before draining")
        require(
            not self.new.audit.state["events"],
            "switch already attempted; inspect status and use explicit rollback before a new deployment",
        )
        self.new.audit.record("switch_intent", plan=plan)
        try:
            if plan["strategy"] == "reuse_gpu":
                self.old.stop(allow_queued=True)
                self.new.preflight()  # prove GPU/ports are actually free, not just drained
            self.new.start()
            self.new.validate()
            if plan["strategy"] == "spare_gpu":
                self.old.audit.record("cutover_drain_intent", candidate=self.new.d.id)
                self.old.quiesce(allow_queued=True)
            result = self.new.activate()
            if plan["strategy"] == "spare_gpu":
                self.old.stop(allow_queued=True)
            self.new.audit.record("active", predecessor_stopped=self.old.d.id)
            return result
        except BaseException:
            try:
                self.rollback()
            except BaseException as recovery:
                raise RuntimeError("switch interrupted; rollback could not prove safe completion; inspect DB/Gateway/Worker before retrying") from recovery
            raise


@contextlib.contextmanager
def locked():
    # Same lock as production Compose replacement: prevent local deployment races.
    with open("/var/lock/vmind-compose-release.lock", "a") as stream:
        fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield


def definition(deployment, predecessor=None):
    return {
        "manifest_json": json.dumps(deployment.data, sort_keys=True, separators=(",", ":")),
        "version_json": json.dumps(deployment.version, sort_keys=True, separators=(",", ":")),
        "compose_source": deployment.compose_path.read_text(),
        "predecessor_id": predecessor or "",
    }


@contextlib.contextmanager
def heartbeat(control, task):
    stopped = threading.Event()

    # Claims never expire or transfer automatically. A failed heartbeat is visible
    # centrally; subsequent audit writes must succeed before further side effects.
    def update():
        while not stopped.wait(15):
            try:
                control.heartbeat(task)
            except Exception:
                pass

    thread = threading.Thread(target=update, daemon=True)
    thread.start()
    try:
        yield
    finally:
        stopped.set()
        thread.join(timeout=1)


def execute(control, gateway, task_id, timeout, recover=False):
    from control import Audit

    # The local lock is acquired BEFORE claiming/recovering. A live predecessor
    # executor therefore cannot be fenced and keep issuing Docker actions.
    with locked(), tempfile.TemporaryDirectory(prefix="vmind-deploy-") as temporary:
        task = control.task(task_id)
        require(task["hostname"] == socket.gethostname(), "run on the declared target host")
        task = control.claim(task_id, task["machine_id"], socket.gethostname(), recover)
        with heartbeat(control, task):
            runner = None
            old = None
            try:
                root = Path(temporary)
                record = control.export(task["deployment_id"], root / "candidate")
                deployment = Deployment(root / "candidate/manifest.json")
                previous = None
                if record["predecessor_id"]:
                    control.export(record["predecessor_id"], root / "previous")
                    previous = Deployment(root / "previous/manifest.json")
                runner = Runner(
                    deployment,
                    gateway,
                    timeout,
                    os.environ.get("MODEL_GATEWAY_API_TOKEN", ""),
                    Audit(control, task, deployment.id),
                    previous,
                )
                if previous:
                    old = Runner(
                        previous,
                        gateway,
                        timeout,
                        runner.api_token,
                        Audit(control, task, previous.id),
                        deployment,
                    )
                action = task["execution_action"]
                if action == "switch-lora":
                    require(old, "recorded predecessor required")
                    switch = Switch(old, runner)
                    plan = switch.plan()
                    runner.validation_supported()
                    result = switch.apply(plan)
                elif action == "rollback":
                    require(old, "recorded predecessor required")
                    result = Switch(old, runner).rollback()
                elif action == "stop":
                    result = runner.stop()
                elif action == "adopt":
                    require(
                        deployment.target.get("adopt_existing") is True,
                        "explicit existing deployment required",
                    )
                    plan = runner.preflight()
                    require(
                        (gateway.instance(deployment.id) or {}).get("eligible"),
                        "adopt requires an eligible existing registration",
                    )
                    require(
                        runner.api_token,
                        "API token required to verify adopted execution version",
                    )
                    runner.audit.record("adopt_intent", plan=plan)
                    runner.accept()
                    runner.audit.record("active")
                    result = {"phase": "active", "adopted": deployment.id}
                else:
                    require(
                        not previous and not deployment.target.get("adopt_existing"),
                        "use switch-lora/adopt for an existing deployment",
                    )
                    if task["activate"]:
                        runner.validation_supported()
                    result = runner.deploy(task["activate"])
                control.finish(task, "succeeded", result)
                return result
            except BaseException as exc:
                # A failed cutover may already have completed rollback. Never try
                # again after an unknown stop or a failed rollback.
                phase = None
                try:
                    phase = control.history(task["deployment_id"])["phase"]
                    if runner and task["execution_action"] == "deploy" and phase not in {"active", "stopped"}:
                        runner.stop()
                        phase = "stopped"
                    safe = phase == "rolled_back" or (phase == "stopped" and task["execution_action"] not in {"switch-lora", "rollback"})
                    control.finish(
                        task,
                        "failed" if safe else "recovery_required",
                        {"error_type": type(exc).__name__, "phase": phase},
                    )
                except Exception:
                    pass  # Retain durable locks; lost acknowledgement never frees execution.
                raise


def main(argv=None):
    from control import Control

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--credentials",
        help="Gateway admin credentials (0600); no database credentials",
    )
    parser.add_argument("--timeout", type=int, default=1800)
    parser.add_argument(
        "command",
        choices=(
            "plan",
            "deploy",
            "switch-lora",
            "rollback",
            "status",
            "stop",
            "adopt",
            "history",
            "export",
            "task",
            "execute",
        ),
    )
    parser.add_argument(
        "manifest",
        help="manifest for plan/deploy/switch-lora/adopt; deployment ID for history/status/export/stop/rollback; task ID for task/execute",
    )
    parser.add_argument("--from", dest="predecessor")
    parser.add_argument("--output", help="new directory for export")
    parser.add_argument("--activate", action="store_true")
    parser.add_argument("--task-id", help="stable operation ID; reuse after an uncertain submission")
    parser.add_argument(
        "--recover",
        action="store_true",
        help="same-host recovery of an interrupted task; stops candidate or rolls back, never repeats deployment",
    )
    args = parser.parse_args(argv)
    require(
        not args.activate or args.command == "deploy",
        "--activate is only valid for deploy",
    )
    require(
        not args.predecessor or args.command in {"plan", "switch-lora"},
        "--from requires plan or switch-lora",
    )
    require(not args.recover or args.command == "execute", "--recover requires execute")
    gateway = Gateway(*load_credentials(args.credentials))
    control = Control(gateway)
    control.ready()
    with tempfile.TemporaryDirectory(prefix="vmind-plan-") as temporary:
        if args.command in {"history", "status"}:
            result = control.history(args.manifest)
        elif args.command == "export":
            require(args.output, "--output required")
            record = control.export(args.manifest, args.output)
            result = {
                "deployment_id": record["id"],
                "directory": args.output,
                "phase_snapshot": record["phase"],
            }
        elif args.command == "task":
            result = control.task(args.manifest)
        elif args.command == "execute":
            result = execute(control, gateway, args.manifest, args.timeout, args.recover)
        elif args.command in {"rollback", "stop"}:
            require(args.task_id, "--task-id required for an idempotent operation")
            result = control.submit(args.task_id, args.command, args.manifest)
        else:
            deployment = Deployment(args.manifest)
            if args.command == "plan":
                previous = None
                if args.predecessor:
                    control.export(args.predecessor, Path(temporary) / "previous")
                    previous = Deployment(Path(temporary) / "previous/manifest.json")
                runner = Runner(deployment, gateway, args.timeout, previous=previous)
                result = Switch(Runner(previous, gateway, args.timeout), runner).plan() if previous else runner.preflight()
            else:
                require(args.task_id, "--task-id required for an idempotent operation")
                require(args.command != "switch-lora" or args.predecessor, "--from required")
                result = control.submit(
                    args.task_id,
                    args.command,
                    deployment.id,
                    definition(deployment, args.predecessor),
                    args.activate,
                )
    print(json.dumps(result, indent=2, default=str))


if __name__ == "__main__":
    try:
        main()
    except (ValueError, RuntimeError, OSError, TimeoutError) as exc:
        sys.exit("deployment failed: " + str(exc))
