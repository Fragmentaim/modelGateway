import contextlib
import json
import socket
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

import manage
from control import Audit, Control
from register import GatewayError


class ControlTest(unittest.TestCase):
    def test_submission_retries_same_operation_after_lost_ack(self):
        gateway = Mock()
        gateway.call.side_effect = [TimeoutError(), {"id": "operation"}]
        with patch("control.time.sleep"):
            self.assertEqual(
                Control(gateway).submit("operation", "stop", "deployment"),
                {"id": "operation"},
            )
        self.assertEqual(gateway.call.call_args_list[0], gateway.call.call_args_list[1])

    def test_audit_retries_same_event_without_duplicate_local_history(self):
        gateway = Mock()
        gateway.call.side_effect = [
            {"events": []},
            GatewayError(503, "unavailable", "unknown commit"),
            {"ok": True},
        ]
        audit = Audit(Control(gateway), {"id": "task", "claim_token": "test-claim"}, "deployment")
        with patch("control.time.sleep"):
            audit.record("stop_intent")
        self.assertEqual(gateway.call.call_args_list[1], gateway.call.call_args_list[2])
        self.assertEqual(len(audit.state["events"]), 1)

    def test_claim_timeout_is_not_automatically_retried(self):
        gateway = Mock()
        gateway.call.side_effect = TimeoutError()
        with self.assertRaises(TimeoutError):
            Control(gateway).claim("task", "machine", "host")
        gateway.call.assert_called_once()

    def test_conflict_is_not_retried(self):
        gateway = Mock()
        gateway.call.side_effect = GatewayError(409, "conflict", "owned")
        with self.assertRaises(GatewayError):
            Control(gateway).submit("task", "stop", "deployment")
        gateway.call.assert_called_once()

    def test_export_uses_remote_definition_and_rejects_path_escape(self):
        for filename in ("../outside", "manifest.json", "compose.json"):
            with self.subTest(filename=filename), tempfile.TemporaryDirectory() as temp:
                gateway = Mock()
                gateway.call.return_value = {
                    "manifest": {"target": {"compose": filename}},
                    "compose_source": '{"services":{}}',
                }
                control = Control(gateway)
                path = Path(temp) / "restored"
                if filename == "compose.json":
                    control.export("deployment", path)
                    self.assertEqual(
                        json.loads((path / "compose.json").read_text()),
                        {"services": {}},
                    )
                else:
                    with self.assertRaises(ValueError):
                        control.export("deployment", path)
                    self.assertFalse(path.exists())

    def test_executor_checks_host_before_claim_and_claim_before_runtime(self):
        control = Mock()
        control.task.return_value = {"hostname": "not-this-host"}
        with (
            patch.object(manage, "locked", contextlib.nullcontext),
            self.assertRaisesRegex(ValueError, "target host"),
        ):
            manage.execute(control, Mock(), "task", 10)
        control.claim.assert_not_called()
        control.task.return_value = {
            "hostname": socket.gethostname(),
            "machine_id": "i-test",
        }
        control.claim.side_effect = TimeoutError()
        with (
            patch.object(manage, "locked", contextlib.nullcontext),
            patch.object(manage, "Runner") as runner,
            self.assertRaises(TimeoutError),
        ):
            manage.execute(control, Mock(), "task", 10)
        runner.assert_not_called()
