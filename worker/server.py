"""Inference worker skeleton.

Phase 1 speaks JSON over HTTP and scores windows with the same threshold rule
as the Go fallback. Phase 2 swaps score() for the ONNX model (and HTTP for
gRPC) without changing what the Go core sends.

POST /score  {"windows": [{"device_id", "t", "ax", "ay", "az", "gx", "gy", "gz"}]}
          -> {"scores": [...], "model_version": "..."}
GET /healthz
"""

import json
import math
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MODEL_VERSION = "threshold-py-v0"
FREE_FALL_G = 0.6
IMPACT_G = 2.5
MAX_GAP_NS = 1_000_000_000


def score(w):
    """1.0 for a free-fall dip followed within MAX_GAP_NS by an impact spike."""
    dip_at = None
    for t, x, y, z in zip(w["t"], w["ax"], w["ay"], w["az"]):
        m = math.sqrt(x * x + y * y + z * z)
        if m < FREE_FALL_G:
            dip_at = t
        elif dip_at is not None and m > IMPACT_G and t - dip_at <= MAX_GAP_NS:
            return 1.0
    return 0.0


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/healthz":
            self._reply(200, {"status": "ok", "model_version": MODEL_VERSION})
        else:
            self._reply(404, {"error": "not found"})

    def do_POST(self):
        if self.path != "/score":
            return self._reply(404, {"error": "not found"})
        try:
            body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
            scores = [score(w) for w in body["windows"]]
        except (ValueError, KeyError, TypeError) as e:
            return self._reply(400, {"error": str(e)})
        self._reply(200, {"scores": scores, "model_version": MODEL_VERSION})

    def _reply(self, code, obj):
        data = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *args):
        pass  # one line per request every 0.5 s is noise


if __name__ == "__main__":
    port = int(os.environ.get("PORT", "8000"))
    print(f"worker listening on :{port}, model={MODEL_VERSION}", flush=True)
    ThreadingHTTPServer(("", port), Handler).serve_forever()
