import copy
import json
import socket
import unittest
from unittest.mock import patch

import manage as e
import test_evaluate as fixtures


class SwitchTest(unittest.TestCase):
    load = fixtures.EvaluationTest.load
    config = fixtures.EvaluationTest.config

    def setUp(self):
        fixtures.EvaluationTest.setUp(self)
        self.old = self.runner
        self.old.d.path = self.root / "old-manifest.json"
        self.old.d.path.write_text(json.dumps(self.old.d.data))
        self.data["deployment_id"] = "body-next"
        self.data["target"]["instance"]["instance_id"] = "body-next"
        self.data["target"]["state_root"] = str(self.root / "body-next")
        self.lora = self.root / "new-lora"
        self.lora.write_text("new lora")
        self.data["version"]["assets"][1]["sha256"] = e.digest(self.lora)
        self.data["target"]["asset_paths"]["lora"] = str(self.lora)
        self.data["acceptance"][0]["expected_generation"]["model_version.sha256"] = e.digest(self.lora)
        candidate = self.load()
        self.new = e.Runner(
            candidate,
            self.gateway,
            timeout=0,
            api_token="secret",
            audit=fixtures.MemoryAudit(),
        )
        self.switch = e.Switch(self.old, self.new)
        row = self.old.d.item.registration()
        self.gateway.rows = [
            {
                "id": row.pop("instance_id"),
                **row,
                "draining": False,
                "eligible": True,
                "healthy": True,
                "reserved_requests": 0,
                "reserved_units": 0,
            }
        ]
        self.running = {self.old.d.id: True}
        self.trace = []
        self.real_call = self.gateway.call
        self.gateway.call = self.gateway_call
        for runner in (self.old, self.new):
            p = patch.object(runner, "containers", side_effect=self.containers)
            p.start()
            self.addCleanup(p.stop)
            p = patch.object(
                runner,
                "accept",
                side_effect=lambda item=None, r=runner: self.accept(r, item),
            )
            p.start()
            self.addCleanup(p.stop)
        for target, kwargs in (
            (
                e,
                {
                    "run": self.command,
                    "stage": self.stage,
                    "read_json": lambda _: {"active_tasks": [], "pending_tasks": 0},
                },
            ),
        ):
            for name, value in kwargs.items():
                p = patch.object(target, name, side_effect=value)
                p.start()
                self.addCleanup(p.stop)
        p = patch.object(socket, "socket")
        p.start()
        self.addCleanup(p.stop)

    def config_for(self, d):
        original = self.d
        self.d = d
        try:
            config = self.config()
        finally:
            self.d = original
        config["services"]["worker"]["deploy"]["resources"]["reservations"]["devices"][0]["device_ids"] = [str(d.item.gpu_devices[0])]
        config["services"]["worker"]["ports"][0]["published"] = "8890" if d.item.gpu_devices[0] == 1 else "8889"
        return config

    def containers(self):
        result = []
        for runner in (self.old, self.new):
            d = runner.d
            if d.id not in self.running:
                continue
            result.append(
                {
                    "Id": d.id,
                    "Image": d.release["image"].split("@")[-1],
                    "State": {"Running": self.running[d.id]},
                    "Config": {
                        "Labels": {
                            "com.docker.compose.project": d.project,
                            "com.docker.compose.service": d.service,
                            "com.docker.compose.config-hash": "pinned",
                        }
                    },
                    "Mounts": [{"Source": str(d.state_root)}],
                    "HostConfig": {"DeviceRequests": [{"DeviceIDs": [str(d.item.gpu_devices[0])], "Count": 0}]},
                }
            )
        return result

    def command(self, argv, **kwargs):
        if argv[:3] == ["docker", "image", "inspect"]:
            return json.dumps([{"Id": argv[-1].split("@")[-1]}])
        if argv[0] == "nvidia-smi":
            index = int(argv[2])
            d = next(r.d for r in (self.old, self.new) if r.d.item.gpu_devices[0] == index)
            busy = any(self.running.get(r.d.id) and r.d.item.gpu_devices[0] == index for r in (self.old, self.new))
            return d.target["gpu_uuid"] + (", 1024" if busy else ", 0")
        project = argv[argv.index("-p") + 1]
        d = next(r.d for r in (self.old, self.new) if r.d.project == project)
        if "config" in argv:
            return "worker pinned" if "--hash" in argv else json.dumps(self.config_for(d))
        if "up" in argv:
            self.trace.append(("start", d.id))
            self.running[d.id] = True
        elif "stop" in argv:
            self.trace.append(("stop", d.id))
            self.running[d.id] = False
        else:
            raise AssertionError(argv)
        return ""

    def stage(self, gateway, item, *args):
        data = item.registration()
        row = {
            "id": data.pop("instance_id"),
            **data,
            "draining": True,
            "eligible": False,
            "healthy": True,
            "reserved_requests": 0,
            "reserved_units": 0,
        }
        gateway.rows = [r for r in gateway.rows if r["id"] != row["id"]] + [row]
        self.trace.append(("stage", row["id"]))

    def gateway_call(self, method, path, body=None):
        if path.endswith("/drain"):
            self.trace.append(("pause" if body["draining"] else "enable", path.split("/")[-2]))
        return self.real_call(method, path, body)

    def accept(self, runner, item):
        self.assertEqual(item.model_key, runner.d.validation_model_id)
        self.assertNotEqual(item.model_key, runner.d.item.model_key)
        self.trace.append(("accept", runner.d.id))
        runner.audit.record("acceptance_passed")

    def spare_gpu(self):
        self.data["target"]["gpu_uuid"] = "GPU-spare"
        self.data["target"]["instance"].update(
            gpu_devices=[1],
            resource_group="us/test/gpu1",
            endpoint="http://worker.example:8890",
        )
        self.new.d = self.load()
        self.switch = e.Switch(self.old, self.new)

    def test_same_gpu_switch_preserves_model_id_and_drains_before_restart(self):
        result = self.switch.apply(self.switch.plan())
        self.assertEqual(result["model_id"], self.old.d.item.model_key)
        self.assertNotEqual(result["version_id"], self.old.d.release_hash)
        self.assertLess(
            self.trace.index(("stop", self.old.d.id)),
            self.trace.index(("start", self.new.d.id)),
        )
        self.assertLess(
            self.trace.index(("accept", self.new.d.id)),
            self.trace.index(("enable", self.new.d.id)),
        )
        self.assertFalse(self.running[self.old.d.id])
        self.assertTrue(self.gateway.instance(self.old.d.id)["draining"])
        self.assertTrue(self.gateway.instance(self.new.d.probe.instance_id)["draining"])

    def test_spare_gpu_validates_before_pausing_old_model(self):
        self.spare_gpu()
        self.switch.apply(self.switch.plan())
        self.assertLess(
            self.trace.index(("accept", self.new.d.id)),
            self.trace.index(("pause", self.old.d.id)),
        )
        self.assertLess(
            self.trace.index(("pause", self.old.d.id)),
            self.trace.index(("enable", self.new.d.id)),
        )

    def test_failed_validation_restores_old_version_and_id(self):
        plan = self.switch.plan()
        with patch.object(self.new, "accept", side_effect=ValueError("wrong checkpoint")):
            with self.assertRaisesRegex(ValueError, "wrong checkpoint"):
                self.switch.apply(plan)
        self.assertTrue(self.running[self.old.d.id])
        self.assertFalse(self.running[self.new.d.id])
        self.assertTrue(self.gateway.instance(self.old.d.id)["eligible"])
        self.assertNotIn(("enable", self.new.d.id), self.trace)
        self.assertEqual(self.new.audit.state["events"][-1]["phase"], "rolled_back")

    def test_unknown_candidate_execution_never_restarts_old_gpu_owner(self):
        plan = self.switch.plan()

        def unknown(item):
            row = next(r for r in self.gateway.rows if r["id"] == item.instance_id)
            row["reserved_requests"] = 1
            raise TimeoutError("unknown execution")

        with patch.object(self.new, "accept", side_effect=unknown):
            with self.assertRaisesRegex(RuntimeError, "could not prove"):
                self.switch.apply(plan)
        self.assertFalse(self.running[self.old.d.id])
        self.assertTrue(self.running[self.new.d.id])
        self.assertTrue(self.gateway.instance(self.new.d.probe.instance_id)["draining"])
        self.assertNotIn(("start", self.old.d.id), self.trace)

    def test_explicit_rollback_after_success_restores_old_routing(self):
        self.switch.apply(self.switch.plan())
        self.assertEqual(self.switch.rollback()["version_id"], self.old.d.release_hash)
        self.assertTrue(self.gateway.instance(self.old.d.id)["eligible"])
        self.assertTrue(self.gateway.instance(self.new.d.id)["draining"])

    def test_cannot_switch_incompatible_schema_or_model_id(self):
        for key, value in (
            ("model_key", "different-model"),
            ("input_schema", {"type": "array"}),
        ):
            with self.subTest(key=key), patch.object(self.new.d.item, key, value):
                with self.assertRaises(ValueError):
                    self.switch.plan()
        self.assertEqual(self.trace, [])

    def test_unmanaged_replica_prevents_partial_version_switch(self):
        other = copy.deepcopy(self.gateway.rows[0])
        other.update(id="other-replica", endpoint="http://other", resource_group="other-gpu")
        self.gateway.rows.append(other)
        with self.assertRaisesRegex(ValueError, "another instance"):
            self.switch.plan()
        self.assertEqual(self.trace, [])

    def test_bound_old_retry_prevents_stopping_old_worker(self):
        plan = self.switch.plan()
        original = self.gateway.call

        def requests(method, path, body=None):
            if "requests?" in path and "instance_id=" + self.old.d.id in path:
                return {"total": 1}
            return original(method, path, body)

        with patch.object(self.gateway, "call", side_effect=requests):
            with self.assertRaisesRegex(ValueError, "pending requests"):
                self.switch.apply(plan)
        self.assertNotIn(("stop", self.old.d.id), self.trace)
        self.assertNotIn(("start", self.new.d.id), self.trace)
        self.assertTrue(self.gateway.instance(self.old.d.id)["eligible"])


if __name__ == "__main__":
    unittest.main()
