#!/usr/bin/env python3
"""Summarize one process's DRIVE READ TIMING records from stdin as JSON."""

import json
import re
import sys
from collections import defaultdict

OPERATIONS = {
    "backend", "metadata", "root-metadata", "list-directory", "list-filtered",
    "media", "media-headers", "oauth", "other", "formats", "about",
}
EVENTS = {
    "phase-start", "attempt-start", "attempt-finished", "phase-finished",
    "http-start", "connection-start", "connection-ready", "dns-start",
    "dns-finished", "tcp-start", "tcp-finished", "tls-start", "tls-finished",
    "request-written", "first-response-byte", "http-headers",
    "body-read-finished", "body-first-read", "body-closed", "backend-created", "root-path-finished",
}
PREFIX = re.compile(
    r"DRIVE READ TIMING id=(\d+) operation=([a-z-]+) event=([a-z-]+) elapsed_ms=(\d+(?:\.\d+)?)"
)
FIELDS = re.compile(r"\b(status|failed|reused|bytes|attempts|retry)=(\d+|true|false)\b")


def summarize(lines):
    records = {}
    for line in lines:
        match = PREFIX.search(line)
        if not match:
            continue
        identifier, operation, event, elapsed = match.groups()
        if operation not in OPERATIONS or event not in EVENTS:
            continue
        identifier = int(identifier)
        row = records.setdefault(identifier, {"id": identifier, "operation": operation, "events": []})
        if event in {"http-start", "phase-start", "backend-created"} and any(item[0] == event for item in row["events"]):
            raise ValueError("Repeated event ID: supply logs from one process only")
        if row["operation"] != operation:
            raise ValueError("Mixed process logs: an event ID has conflicting operations")
        row["events"].append((event, float(elapsed), dict(FIELDS.findall(line[match.end():]))))
    totals = defaultdict(lambda: {"http_requests": 0, "headers_ms": 0.0, "pacer_wait_ms": 0.0, "api_attempts": 0})
    rows = []
    for row in sorted(records.values(), key=lambda item: item["id"]):
        output = {"id": row["id"], "operation": row["operation"]}
        previous_end = 0.0
        wait = 0.0
        attempts = 0
        for event, elapsed, fields in row["events"]:
            if event == "phase-start":
                previous_end = elapsed
            elif event == "attempt-start":
                wait += max(0.0, elapsed - previous_end)
                attempts += 1
            elif event == "attempt-finished":
                previous_end = elapsed
            elif event == "phase-finished":
                output["phase_ms"] = elapsed
                output["pacer_wait_ms"] = round(wait, 3)
                output["attempts"] = attempts
                totals[row["operation"]]["pacer_wait_ms"] += wait
                totals[row["operation"]]["api_attempts"] += attempts
            elif event == "http-headers":
                output["headers_ms"] = elapsed
                output["status"] = int(fields.get("status", "0"))
                output["failed"] = fields.get("failed") == "true"
                totals[row["operation"]]["http_requests"] += 1
                totals[row["operation"]]["headers_ms"] += elapsed
            elif event == "connection-ready":
                output["connection_ready_ms"] = elapsed
                output["reused"] = fields.get("reused") == "true"
            elif event == "first-response-byte":
                output["first_response_ms"] = elapsed
            elif event == "body-first-read":
                output["first_body_read_ms"] = elapsed
            elif event == "body-closed":
                output["body_elapsed_ms"] = elapsed
                output["bytes_read"] = int(fields.get("bytes", "0"))
            elif event == "backend-created":
                output["backend_created_ms"] = elapsed
            elif event == "root-path-finished":
                output["backend_ready_ms"] = elapsed
        rows.append(output)
    return {
        "records": rows,
        "totals": {operation: {key: round(value, 3) if isinstance(value, float) else value for key, value in counts.items()}
                   for operation, counts in sorted(totals.items())},
    }


if __name__ == "__main__":
    json.dump(summarize(sys.stdin), sys.stdout, indent=2)
    sys.stdout.write("\n")
