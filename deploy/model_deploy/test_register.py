import json
import os
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import register


class FakeGateway:
    """In-memory subset of the modelGateway admin API used by register.py."""

    def __init__(self):
        self.groups, self.instances, self.calls = {}, {}, []
        self.worker_ready = True
        self.honor_initially_draining = True

    def handle(self, method, path, body):
        self.calls.append((method, path))
        if path == "/v1/resource-groups" and method == "GET":
            return 200, list(self.groups.values())
        if path == "/v1/resource-groups" and method == "POST":
            if body["id"] in self.groups or any(g["machine_id"] == body["machine_id"] and g["gpu_device"] == body["gpu_device"] for g in self.groups.values()):
                return 409, {"error": {"code": "resource_group_exists", "message": "exists"}}
            self.groups[body["id"]] = dict(body)
            return 201, body
        if path == "/v1/instances/probe":
            return (
                (200, {"ready": True})
                if self.worker_ready
                else (
                    502,
                    {"error": {"code": "health_probe_failed", "message": "down"}},
                )
            )
        if path == "/v1/instances/register":
            if body["resource_group"] not in self.groups:
                return 404, {"error": {"code": "resource_group_not_found", "message": "missing"}}
            old = self.instances.get(body["instance_id"])
            record = {k: v for k, v in body.items() if k not in {"instance_id", "initially_draining"}}
            if old:
                same = all(old.get(k) == v for k, v in record.items())
                return (200, {"instance_id": body["instance_id"]}) if same else (409, {"error": {"code": "conflict", "message": "conflict"}})
            if any(i["endpoint"] == body["endpoint"] and not i.get("draining") for i in self.instances.values()):
                return 409, {"error": {"code": "conflict", "message": "conflict"}}
            self.instances[body["instance_id"]] = {
                "id": body["instance_id"],
                **record,
                "draining": body.get("initially_draining", False) and self.honor_initially_draining,
                "healthy": True,
                "eligible": True,
                "reserved_requests": 0,
                "reserved_units": 0,
            }
            return 200, {"instance_id": body["instance_id"]}
        if path == "/v1/instances" and method == "GET":
            return 200, [dict(row, eligible=row["healthy"] and not row.get("draining")) for row in self.instances.values()]
        if path.endswith("/drain"):
            row = self.instances[path.split("/")[3]]
            row["draining"] = body["draining"]
            return 200, {"draining": body["draining"]}
        return 404, {"error": {"code": "not_found", "message": path}}


def serve(handler_factory):
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler_factory)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server, f"http://127.0.0.1:{server.server_port}"


class RegisterTest(unittest.TestCase):
    def setUp(self):
        self.fake = FakeGateway()
        fake = self.fake

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def _reply(self, method):
                length = int(self.headers.get("Content-Length") or 0)
                body = json.loads(self.rfile.read(length)) if length else None
                if self.headers.get("Authorization") != "Bearer secret":
                    status, payload = (
                        401,
                        {"error": {"code": "unauthorized", "message": "no"}},
                    )
                else:
                    status, payload = fake.handle(method, self.path, body)
                raw = json.dumps(payload).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def do_GET(self):
                self._reply("GET")

            def do_POST(self):
                self._reply("POST")

        class Worker(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                raw = json.dumps({"ready": fake.worker_ready}).encode()
                self.send_response(200)
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

        self.server, url = serve(Handler)
        self.worker, worker_url = serve(Worker)
        for server in (self.server, self.worker):
            self.addCleanup(server.server_close)
            self.addCleanup(server.shutdown)
        self.gateway = register.Gateway(url, "secret", timeout=5)
        self.item = register.Instance.parse(
            {
                "instance_id": "l20-3-wardrobe-gpu0",
                "model_key": "Vmind_wardrobe_v1.0",
                "protocol": "garment_extraction",
                "endpoint": "https://wardrobe.example/gpu0/",
                "health_url": "https://wardrobe.example/gpu0/v1/service/status",
                "resource_group": "cn/l20-3/gpu0",
                "machine_id": "l20-3",
                "gpu_devices": [0],
                "local_health_url": worker_url + "/v1/service/status",
            }
        )

    def test_creates_group_registers_and_is_idempotent(self):
        receipt = register.ensure(self.gateway, self.item, timeout=5, interval=0.01)
        self.assertEqual(receipt["group"], "created")
        self.assertTrue(receipt["eligible"])
        self.assertEqual(self.fake.groups["cn/l20-3/gpu0"]["gpu_device"], 0)
        self.assertEqual(
            self.fake.instances["l20-3-wardrobe-gpu0"]["endpoint"],
            "https://wardrobe.example/gpu0",
        )
        posts = sum(1 for call in self.fake.calls if call == ("POST", "/v1/resource-groups"))
        receipt = register.ensure(self.gateway, self.item, timeout=5, interval=0.01)
        self.assertEqual(receipt["group"], "existing")
        self.assertEqual(
            posts,
            sum(1 for call in self.fake.calls if call == ("POST", "/v1/resource-groups")),
        )

    def test_resumes_drained_instance(self):
        register.ensure(self.gateway, self.item, timeout=5, interval=0.01)
        self.assertEqual(
            register.drain(self.gateway, self.item.instance_id, 5, 0.01, True)["status"],
            "drained",
        )
        self.assertTrue(self.fake.instances[self.item.instance_id]["draining"])
        self.assertTrue(register.ensure(self.gateway, self.item, timeout=5, interval=0.01)["eligible"])
        self.assertFalse(self.fake.instances[self.item.instance_id]["draining"])

    def test_stage_and_retry_never_resume(self):
        for _ in range(2):
            row = register.stage(self.gateway, self.item, 5, 0.01)
            self.assertTrue(row["draining"])
            self.assertFalse(row["eligible"])
        self.assertFalse(any(path.endswith("/drain") for _, path in self.fake.calls))

    def test_stage_rejects_active_instance_without_mutation(self):
        register.ensure(self.gateway, self.item, 5, 0.01)
        self.fake.calls.clear()
        with self.assertRaisesRegex(ValueError, "already draining"):
            register.stage(self.gateway, self.item, 5, 0.01)
        self.assertFalse(any(method == "POST" for method, _ in self.fake.calls))

    def test_gateway_ignoring_initial_drain_fails_and_pauses_candidate(self):
        self.fake.honor_initially_draining = False
        with self.assertRaisesRegex(RuntimeError, "did not honor"):
            register.stage(self.gateway, self.item, 5, 0.01)
        self.assertTrue(self.fake.instances[self.item.instance_id]["draining"])

    def test_changed_settings_fail_without_editing(self):
        register.ensure(self.gateway, self.item, timeout=5, interval=0.01)
        self.item.endpoint = "https://wardrobe.example/other"
        with self.assertRaises(SystemExit) as raised:
            register.ensure(self.gateway, self.item, timeout=5, interval=0.01)
        self.assertIn("endpoint", str(raised.exception))
        self.assertEqual(
            self.fake.instances[self.item.instance_id]["endpoint"],
            "https://wardrobe.example/gpu0",
        )

    def test_card_bound_to_another_group_fails(self):
        self.fake.groups["legacy/gpu0"] = {
            "id": "legacy/gpu0",
            "machine_id": "l20-3",
            "gpu_device": 0,
            "capacity_units": 1,
        }
        with self.assertRaises(SystemExit) as raised:
            register.ensure(self.gateway, self.item, timeout=5, interval=0.01)
        self.assertIn("legacy/gpu0", str(raised.exception))
        self.assertEqual(self.fake.instances, {})

    def test_unready_worker_times_out_before_registration(self):
        self.fake.worker_ready = False
        with self.assertRaises(TimeoutError):
            register.ensure(self.gateway, self.item, timeout=0.2, interval=0.05)
        self.assertEqual(self.fake.groups, {})
        self.assertEqual(self.fake.instances, {})

    def test_drain_unknown_instance_is_noop(self):
        self.assertEqual(
            register.drain(self.gateway, "missing", 1, 0.01, True)["status"],
            "not_registered",
        )

    def test_credentials_file_must_be_private(self):
        with tempfile.NamedTemporaryFile("w", delete=False) as handle:
            handle.write("MODEL_GATEWAY_URL=https://gw.example/\nexport MODEL_GATEWAY_ADMIN_TOKEN='t'\n")
        self.addCleanup(os.unlink, handle.name)
        os.chmod(handle.name, 0o644)
        with self.assertRaises(SystemExit):
            register.load_credentials(handle.name)
        os.chmod(handle.name, 0o600)
        self.assertEqual(register.load_credentials(handle.name), ("https://gw.example", "t"))

    def test_manifest_defaults_and_filter(self):
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as handle:
            json.dump(
                {
                    "defaults": {
                        "model_key": "M",
                        "protocol": "garment_extraction",
                        "machine_id": "m",
                    },
                    "instances": [
                        {
                            "instance_id": f"i{n}",
                            "endpoint": f"https://h/gpu{n}",
                            "health_url": f"https://h/gpu{n}/s",
                            "resource_group": f"m/gpu{n}",
                            "gpu_devices": [n],
                        }
                        for n in (0, 1)
                    ],
                },
                handle,
            )
        self.addCleanup(os.unlink, handle.name)
        items = register.load_manifest(handle.name, ["i1"])
        self.assertEqual(
            [(i.instance_id, i.model_key, i.gpu_devices) for i in items],
            [("i1", "M", [1])],
        )
        with self.assertRaises(ValueError):
            register.load_manifest(handle.name, ["i9"])


if __name__ == "__main__":
    unittest.main()
