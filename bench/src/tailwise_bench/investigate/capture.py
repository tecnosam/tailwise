"""The exact requests Tailwise sends a System One model, captured without
calling one.

A local HTTP server stands in for the model: it records every request body
and answers every question with a fixed value. Tailwise runs against it with
the settings under test, and each run is sent through the same OTLP endpoint
the benchmark uses, one at a time, so each captured request belongs to the
run just sent.
"""

from __future__ import annotations

import json
import subprocess
import threading
import time
from collections.abc import Iterator
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

import httpx
import yaml

from ..otel import run_trace
from ..types import Run


class _Recorder(ThreadingHTTPServer):
    last: dict[str, Any] | None = None
    lock = threading.Lock()


class _Handler(BaseHTTPRequestHandler):
    server: _Recorder

    def log_message(self, *args: Any) -> None:
        pass

    def _reply(self, body: dict[str, Any]) -> None:
        data = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self) -> None:  # /v1/models, Tailwise's readiness probe
        self._reply({"models": [{"name": "capture"}]})

    def do_POST(self) -> None:
        req = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        with self.server.lock:
            self.server.last = req
        answers = {q: ({"type": "noul", "noul": 0.1} if spec.get("type") == "noul"
                       else {"type": "choice", "choice": "correct", "confidence": 1.0, "probabilities": {}})
                   for q, spec in req.get("questions", {}).items()}
        self._reply({"model": "capture", "answers": answers, "usage": {"input_tokens": 0, "output_tokens": 0}})


@contextmanager
def fake_model(port: int) -> Iterator[_Recorder]:
    srv = _Recorder(("127.0.0.1", port), _Handler)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    try:
        yield srv
    finally:
        srv.shutdown()


@contextmanager
def tailwise(binary: Path, workdir: Path, port: int, model_port: int, scorer: dict[str, Any]) -> Iterator[str]:
    """Tailwise pointed at the fake model, with the given scorer settings."""
    conf = workdir / f"capture-{port}.yaml"
    conf.write_text(yaml.safe_dump({
        "server": {"addr": f"127.0.0.1:{port}"},
        "otlp": {"grpc_addr": "", "http_addr": ""},
        "scorer": {"kind": "kev", "endpoint": f"http://127.0.0.1:{model_port}/v1/systemone", "model": "capture",
                   "max_retries": 0, **scorer},
        "policy": {"random_floor": 0},
        "exporters": [],
    }))
    log = (workdir / f"capture-{port}.log").open("wb")
    proc = subprocess.Popen([str(binary), "-config", str(conf)], stdout=log, stderr=subprocess.STDOUT)
    url = f"http://127.0.0.1:{port}"
    try:
        for _ in range(120):
            try:
                if httpx.get(url + "/readyz", timeout=2).status_code == 200:
                    break
            except httpx.TransportError:
                pass
            if proc.poll() is not None:
                raise RuntimeError(f"Tailwise exited; see {log.name}")
            time.sleep(0.25)
        yield url
    finally:
        proc.terminate()
        proc.wait(timeout=30)
        log.close()


def capture(runs: list[Run], binary: Path, workdir: Path, scorer: dict[str, Any],
            port: int = 18190, model_port: int = 18191) -> dict[str, dict[str, Any]]:
    """trace_id -> the request Tailwise sent the model for that run, or
    {"error": ...} when Tailwise answered without calling it."""
    out: dict[str, dict[str, Any]] = {}
    with fake_model(model_port) as rec, tailwise(binary, workdir, port, model_port, scorer) as url, \
            httpx.Client(base_url=url, timeout=60) as client:
        for r in runs:
            payload, _ = run_trace(r)
            with rec.lock:
                rec.last = None
            resp = client.post("/v1/otlp/traces:score", json=payload)
            with rec.lock:
                got = rec.last
            out[r.trace_id] = got if got is not None and resp.status_code == 200 else \
                {"error": f"HTTP {resp.status_code}: {resp.text[:200]}"}
    return out
