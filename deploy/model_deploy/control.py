"""Gateway deployment API client. GPU executors never receive a database DSN."""

import json
import time
import urllib.error
import uuid
from pathlib import Path

from register import GatewayError

PREFIX = "/v1/admin/deployment-tasks"


class Control:
    def __init__(self, gateway):
        self.gateway = gateway

    def call(self, method, path, body=None, retry=False):
        # Only repeat operations with stable task/event identity. Claim is deliberately
        # not retried: a lost response requires explicit same-host recovery.
        for attempt in range(3):
            try:
                return self.gateway.call(method, path, body)
            except (urllib.error.URLError, TimeoutError, GatewayError) as exc:
                if not retry or attempt == 2 or (isinstance(exc, GatewayError) and exc.status < 500):
                    raise
                time.sleep(attempt + 1)

    def ready(self):
        return self.call("GET", "/v1/admin/deployments-ready")

    def history(self, identity):
        return self.call("GET", "/v1/admin/deployments/" + identity)

    def export(self, identity, directory):
        record = self.history(identity)
        directory = Path(directory)
        filename = record["manifest"]["target"]["compose"]
        if Path(filename).name != filename or filename in ("manifest.json", ".", ".."):
            raise ValueError("invalid stored Compose filename")
        directory.mkdir(mode=0o700)
        (directory / filename).write_text(record["compose_source"])
        (directory / "manifest.json").write_text(json.dumps(record["manifest"], indent=2) + "\n")
        return record

    def submit(self, task_id, action, deployment_id, definition=None, activate=False):
        body = {
            "id": task_id,
            "action": action,
            "deployment_id": deployment_id,
            "activate": activate,
        }
        if definition is not None:
            body["definition"] = definition
        return self.call("POST", PREFIX, body, retry=True)

    def task(self, identity):
        return self.call("GET", PREFIX + "/" + identity)

    def claim(self, identity, machine, hostname, recover=False):
        return self.call(
            "POST",
            PREFIX + "/" + identity + "/claim",
            {"machine_id": machine, "hostname": hostname, "recover": recover},
        )

    def heartbeat(self, task):
        return self.call(
            "POST",
            PREFIX + "/" + task["id"] + "/heartbeat",
            {"claim_token": task["claim_token"]},
            retry=True,
        )

    def finish(self, task, status, result):
        return self.call(
            "POST",
            PREFIX + "/" + task["id"] + "/finish",
            {"claim_token": task["claim_token"], "status": status, "result": result},
            retry=True,
        )


class Audit:
    def __init__(self, control, task, identity):
        self.control, self.task, self.identity = control, task, identity
        self.state = {"events": [{"phase": e["phase"], **e["summary"]} for e in control.history(identity)["events"]]}

    def record(self, phase, **summary):
        event = {
            "event_id": str(uuid.uuid4()),
            "deployment_id": self.identity,
            "phase": phase,
            "summary": summary,
        }
        self.control.call(
            "POST",
            PREFIX + "/" + self.task["id"] + "/events",
            {"claim_token": self.task["claim_token"], "event": event},
            retry=True,
        )
        self.state["events"].append({"phase": phase, **summary})
