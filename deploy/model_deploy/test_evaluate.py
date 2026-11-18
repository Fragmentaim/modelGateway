import copy
import io
import json
import os
import socket
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import manage as e


class MemoryGateway:
    base = "https://gateway.example"

    def __init__(self):
        self.rows, self.group_rows, self.calls = [], [], []
        self.pending = 0
        self.detail = {}

    def instances(self):
        return copy.deepcopy(self.rows)

    def instance(self, identity):
        return next((r for r in self.instances() if r["id"] == identity), None)

    def groups(self):
        return self.group_rows

    def call(self, method, path, body=None):
        self.calls.append((method, path, body))
        if path.endswith("/drain"):
            row = next(r for r in self.rows if r["id"] == path.split("/")[-2])
            row["draining"] = body["draining"]
            row["eligible"] = not body["draining"]
        elif "requests?" in path:
            return {"total": self.pending}
        elif "/requests/" in path:
            return self.detail


class MemoryAudit:
    def __init__(self):
        self.state = {"events": []}

    def record(self, phase, **values):
        self.state["events"].append({"phase": phase, **values})


class EvaluationTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.asset = self.root / "weights"
        self.asset.write_text("immutable weights")
        self.case = self.root / "input.json"
        self.case.write_text('{"prompt":"evaluation", "n":1}')
        self.compose = self.root / "compose.json"
        self.compose.write_text('{"services": {"worker": {"image": "template"}}}')
        self.data = {
            "deployment_id": "eval-body-test",
            "model_id": "Vmind_bodymodel_v1.0",
            "version": {
                "image": "registry.example/image@sha256:" + "a" * 64,
                "engine": "qwen",
                "base_model": "qwen-turbo-pinned",
                "binding": {"profile": "body_model"},
                "command": [],
                "entrypoint": [],
                "environment": {},
                "assets": [
                    {
                        "name": role,
                        "sha256": e.digest(self.asset),
                        "role": role,
                        "source": "s3://example/weights",
                    }
                    for role in ("weights", "lora")
                ],
                "cases": [
                    {
                        "path": str(self.case),
                        "sha256": e.digest(self.case),
                        "require_pe": False,
                        "expected_generation": {"model_version.sha256": e.digest(self.asset)},
                        "expected_size": [2, 2],
                    }
                ],
            },
            "target": {
                "compose": "compose.json",
                "service": "worker",
                "hostname": socket.gethostname(),
                "gpu_uuid": "GPU-test",
                "state_root": str(self.root / "eval-body-test"),
                "idle_url": "http://localhost:8889/v1/service/status",
                "instance": {
                    "instance_id": "eval-body-test",
                    "model_key": "",
                    "protocol": "image_generation",
                    "endpoint": "http://worker.example:8889",
                    "health_url": "http://worker.example:8889/health",
                    "local_health_url": "http://localhost:8889/health",
                    "resource_group": "us/test/gpu0",
                    "machine_id": "test",
                    "gpu_devices": [0],
                    "artifact_digest": "sha256-" + "b" * 64,
                    "input_schema": {"type": "object"},
                },
            },
        }
        self.data["acceptance"] = self.data["version"].pop("cases")
        self.data["target"]["compose_sha256"] = e.digest(self.compose)
        self.data["target"]["asset_paths"] = {
            "weights": str(self.asset),
            "lora": str(self.asset),
        }
        self.validator = self.root / "validator.py"
        self.validator.write_text(
            "import argparse,json,pathlib; p=argparse.ArgumentParser(); p.add_argument('--media'); p.add_argument('--case'); a=p.parse_args(); assert pathlib.Path(a.media).read_bytes().startswith(bytes([137,80,78,71])); print(json.dumps({'size':[2,2]}))"
        )
        self.data["version"]["output_validator"] = "validator"
        self.data["version"]["assets"].append(
            {
                "name": "validator",
                "role": "validator",
                "sha256": e.digest(self.validator),
                "source": "git://example/test-validator.py",
            }
        )
        self.data["target"]["asset_paths"]["validator"] = str(self.validator)
        self.d = self.load()
        self.gateway, self.audit = MemoryGateway(), MemoryAudit()
        self.runner = e.Runner(self.d, self.gateway, timeout=0, api_token="secret", audit=self.audit)

    def load(self):
        self.data["target"]["instance"]["model_key"] = self.data["model_id"]
        for field in ("health_url", "local_health_url"):
            self.data["target"]["instance"][field] = "http://worker.example:8889/v1/images/models/" + self.data["model_id"] + "/health"
        self.data["target"]["instance"]["artifact_digest"] = "sha256-" + e.fingerprint(self.data["version"])
        path = self.root / "manifest.json"
        path.write_text(json.dumps(self.data))
        return e.Deployment(path)

    def row(self):
        item = self.d.item.registration()
        return {
            "id": item.pop("instance_id"),
            **item,
            "draining": True,
            "eligible": False,
            "reserved_units": 0,
            "reserved_requests": 0,
        }

    def config(self):
        return {
            "services": {
                "worker": {
                    "image": self.d.release["image"],
                    "user": f"{os.geteuid()}:{os.getegid()}",
                    "environment": {
                        "MODEL_GATEWAY_MANAGED": "1",
                        "VMIND_GENERATION_STATE": "/state/generation",
                        "VMIND_WARMUP_READY_FILE": "/state/ready",
                        "VMIND_IMAGE_MODELS": json.dumps(
                            {
                                self.d.item.model_key: {"profile": "body_model"},
                                self.d.validation_model_id: {"profile": "body_model"},
                            }
                        ),
                    },
                    "deploy": {"resources": {"reservations": {"devices": [{"device_ids": ["0"], "capabilities": ["gpu"]}]}}},
                    "ports": [{"published": "8889", "target": 8000}],
                    "volumes": [
                        {
                            "type": "bind",
                            "source": str(self.root),
                            "target": "/weights",
                            "read_only": True,
                        },
                        {
                            "type": "bind",
                            "source": str(self.d.state_root),
                            "target": "/state",
                        },
                    ],
                }
            }
        }

    def test_version_change_preserves_model_key_but_changes_execution_identity(self):
        self.data["version"]["image"] = "registry.example/image@sha256:" + "c" * 64
        other = self.load()
        self.assertEqual(other.item.model_key, self.d.item.model_key)
        self.assertNotEqual(other.release_hash, self.d.release_hash)
        self.assertNotEqual(other.request_id(0), self.d.request_id(0))
        self.assertEqual(e.Deployment(self.d.path).request_id(0), other.request_id(0))

    def test_machine_and_case_changes_do_not_change_model_version(self):
        self.data["target"]["hostname"] = "another-machine"
        self.data["target"]["gpu_uuid"] = "GPU-other"
        self.data["acceptance"][0]["expected_size"] = [4, 4]
        other = self.load()
        self.assertEqual(other.release_hash, self.d.release_hash)
        self.assertNotEqual(other.identity, self.d.identity)

    def test_inference_environment_cannot_change_outside_model_version(self):
        config = self.config()
        config["services"]["worker"]["environment"]["BODYMODEL_CHECKPOINT"] = "unrecorded-version"
        with (
            patch.object(e, "run", return_value=json.dumps(config)),
            self.assertRaisesRegex(ValueError, "immutable version"),
        ):
            self.d.config()

    def test_assets_changed_rejected(self):
        self.asset.write_text("different checkpoint")
        with self.assertRaisesRegex(ValueError, "digest mismatch"):
            self.d.check_files()

    def test_model_id_must_match_gateway_model_key(self):
        self.data["target"]["instance"]["model_key"] = "different-model"
        self.d.path.write_text(json.dumps(self.data))
        with self.assertRaisesRegex(ValueError, "model_id must equal"):
            e.Deployment(self.d.path)

    def test_existing_other_alias_or_gpu_registration_is_never_modified(self):
        row = self.row()
        row.update(id="production", model_key="production", endpoint="http://other")
        self.gateway.rows = [row]
        with self.assertRaisesRegex(ValueError, "unused GPU"):
            e.check_gateway(self.gateway, self.d)
        self.assertEqual(self.gateway.calls, [])

    def test_existing_registration_drift_rejected(self):
        row = self.row()
        row["artifact_digest"] = "sha256-" + "f" * 64
        self.gateway.rows = [row]
        with self.assertRaisesRegex(ValueError, "differs"):
            e.check_gateway(self.gateway, self.d)

    def test_no_resource_group_per_version_or_capacity_increase(self):
        self.gateway.group_rows = [
            {
                "id": "old-gpu-group",
                "machine_id": "test",
                "gpu_device": 0,
                "capacity_units": 1,
            }
        ]
        with self.assertRaisesRegex(ValueError, "physical GPU"):
            e.check_gateway(self.gateway, self.d)

    def test_retired_evaluation_registration_does_not_permanently_claim_gpu(self):
        retired = self.row()
        retired.update(
            id="eval-retired",
            model_key="eval_old_version",
            endpoint="http://old-endpoint",
            healthy=False,
        )
        self.gateway.rows = [retired]
        self.assertIsNone(e.check_gateway(self.gateway, self.d))
        self.assertFalse(self.gateway.calls)
        retired["reserved_units"] = 1
        with self.assertRaises(ValueError):
            e.check_gateway(self.gateway, self.d)

    def test_compose_binding_uses_placeholder_without_hash_cycle(self):
        self.assertIn("MODEL_ID=" + self.d.item.model_key, self.d.compose("config"))
        with patch.object(e, "run", return_value=json.dumps(self.config())):
            self.d.config()

    def test_state_or_gpu_sharing_rejected(self):
        for change in ("state", "gpu", "alias"):
            with self.subTest(change=change):
                config = self.config()
                svc = config["services"]["worker"]
                if change == "state":
                    svc["volumes"][1]["source"] = "/production/state"
                elif change == "gpu":
                    svc["deploy"]["resources"]["reservations"]["devices"][0]["device_ids"] = ["1"]
                else:
                    svc["environment"]["VMIND_IMAGE_MODELS"] = '{"production": {}}'
                with (
                    patch.object(e, "run", return_value=json.dumps(config)),
                    self.assertRaises(ValueError),
                ):
                    self.d.config()

    def test_plan_is_read_only_and_busy_gpu_rejected(self):
        def command(argv, **kw):
            if argv[0] == "nvidia-smi":
                return "GPU-test, 0"
            return "[]"

        with (
            patch.object(
                self.d,
                "config",
                return_value=(self.config(), self.config()["services"]["worker"]),
            ),
            patch.object(self.runner, "containers", return_value=[]),
            patch.object(e, "run", side_effect=command) as run,
            patch.object(socket, "socket"),
        ):
            self.assertEqual(self.runner.preflight()["instance_id"], self.d.id)
            self.assertFalse(self.gateway.calls)
            self.assertFalse(self.audit.state["events"])
            self.assertFalse(any("up" in call.args[0] for call in run.call_args_list))
            with (
                patch.object(e, "run", return_value="GPU-test, 1024"),
                self.assertRaisesRegex(ValueError, "not idle"),
            ):
                self.runner.preflight()

    def staged(self, gateway, item, *args):
        data = item.registration()
        row = {
            "id": data.pop("instance_id"),
            **data,
            "draining": True,
            "eligible": False,
            "reserved_requests": 0,
            "reserved_units": 0,
        }
        self.gateway.rows = [r for r in self.gateway.rows if r["id"] != row["id"]] + [row]

    def test_deploy_defaults_to_staged_without_submission(self):
        with (
            patch.object(self.runner, "preflight", return_value={}),
            patch.object(self.runner, "containers", return_value=[]),
            patch.object(self.runner, "owned", return_value=None),
            patch.object(e, "run") as run,
            patch.object(e, "stage", side_effect=self.staged),
            patch.object(self.runner, "prepare_state"),
            patch.object(self.runner, "accept") as accept,
        ):
            self.assertEqual(self.runner.deploy(), {"phase": "staged", "draining": True})
            accept.assert_not_called()
            self.assertTrue(any("up" in call.args[0] for call in run.call_args_list))
            self.assertFalse(self.gateway.calls)

    def test_activation_failure_drains_without_stopping_worker(self):
        with (
            patch.object(self.d, "config"),
            patch.object(self.runner, "preflight", return_value={}),
            patch.object(self.runner, "containers", return_value=[]),
            patch.object(self.runner, "owned", return_value={"State": {"Running": True}}),
            patch.object(e, "run") as run,
            patch.object(e, "stage", side_effect=self.staged),
            patch.object(self.runner, "accept", side_effect=TimeoutError),
        ):
            with self.assertRaises(TimeoutError):
                self.runner.deploy(activate=True)
            self.assertTrue(self.gateway.rows[0]["draining"])
            run.assert_not_called()
            self.assertNotIn("active", [event["phase"] for event in self.audit.state["events"]])

    def test_database_intent_failure_prevents_start(self):
        with (
            patch.object(self.runner, "preflight", return_value={}),
            patch.object(self.audit, "record", side_effect=RuntimeError("database unavailable")),
            patch.object(e, "run") as run,
        ):
            with self.assertRaisesRegex(RuntimeError, "database unavailable"):
                self.runner.deploy()
            run.assert_not_called()

    def test_stop_waits_for_gateway_and_worker_and_keeps_state(self):
        self.gateway.rows = [self.row()]
        with (
            patch.object(self.d, "config"),
            patch.object(self.runner, "containers", return_value=[]),
            patch.object(self.runner, "owned", return_value={"State": {"Running": True}}),
            patch.object(e, "read_json", return_value={"active_tasks": [], "pending_tasks": 0}),
            patch.object(e, "run") as run,
        ):
            result = self.runner.stop()
            self.assertTrue(result["state_preserved"])
            self.assertEqual(run.call_args.args[0][-4:], ["stop", "--timeout", "60", "worker"])

    def test_stop_refuses_queued_reserved_busy_and_unknown_work(self):
        for problem in ("queued", "reserved", "busy", "unknown"):
            with self.subTest(problem=problem):
                self.gateway.rows = [self.row()]
                self.gateway.pending = int(problem == "queued")
                self.gateway.rows[0]["reserved_requests"] = int(problem == "reserved")
                worker = {
                    "active_tasks": ["running"] if problem == "busy" else [],
                    "pending_tasks": 0,
                }
                if problem == "unknown":
                    worker = {"ready": True}
                with (
                    patch.object(self.d, "config"),
                    patch.object(self.runner, "containers", return_value=[]),
                    patch.object(self.runner, "owned", return_value={"State": {"Running": True}}),
                    patch.object(e, "read_json", return_value=worker),
                    patch.object(e, "run") as run,
                ):
                    with self.assertRaises((ValueError, TimeoutError)):
                        self.runner.stop()
                    self.assertTrue(self.gateway.rows[0]["draining"])
                    run.assert_not_called()

    def test_acceptance_checks_pe_weights_dispatch_and_output(self):
        from PIL import Image

        raw = io.BytesIO()
        Image.new("RGB", (2, 2)).save(raw, format="PNG")
        good = {
            "state": "succeeded",
            "instance_id": self.d.id,
            "result": {
                "generation": {
                    "model_version": {"sha256": e.digest(self.asset)},
                    "pe_status": "applied",
                }
            },
        }
        self.d.release["cases"][0]["require_pe"] = True
        for problem in (None, "pe", "weight", "dispatch", "image"):
            with self.subTest(problem=problem):
                self.gateway.detail = copy.deepcopy(good)
                if problem == "pe":
                    self.gateway.detail["result"]["generation"]["pe_status"] = "fallback"
                elif problem == "weight":
                    self.gateway.detail["result"]["generation"]["model_version"]["sha256"] = "wrong"
                elif problem == "dispatch":
                    self.gateway.detail["instance_id"] = "production"
                stream = io.BytesIO(b"bad" if problem == "image" else raw.getvalue())
                with (
                    patch.object(e, "Gateway") as api,
                    patch.object(e.OPENER, "open", return_value=stream),
                ):
                    if problem:
                        with self.assertRaises((ValueError, OSError, RuntimeError)):
                            self.runner.accept()
                    else:
                        self.runner.accept()
                        self.assertEqual(
                            api.return_value.call_with_headers.call_args.args[1],
                            "/v1/images/generations",
                        )
                        self.assertEqual(self.audit.state["events"][-1]["phase"], "acceptance_passed")


if __name__ == "__main__":
    unittest.main()
