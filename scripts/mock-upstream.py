#!/usr/bin/env python3
"""Small local-only upstream used by scripts/start-local.sh.

It serves only checked-in synthetic fixtures and never logs request bodies.
"""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlsplit
import json
import os
import time

ROOT = Path(__file__).resolve().parents[1] / "tests" / "fixtures"

# Per-line SSE delay is configurable so a browser can observe the live stream
# projection while records are still flowing (default keeps tests quick).
SSE_DELAY = float(os.environ.get("MOCK_SSE_DELAY_MS", "30")) / 1000.0
CASES = {
    "/v1/responses": (
        "responses/json/response.json",
        "responses/sse/response.sse",
        "application/json",
        "text/event-stream; charset=utf-8",
    ),
    "/v1/chat/completions": (
        "chat_completions/json/response.json",
        "chat_completions/sse/response.sse",
        "application/json",
        "text/event-stream; charset=utf-8",
    ),
    "/v1/messages": (
        "anthropic_messages/json/response.json",
        "anthropic_messages/sse/response.sse",
        "application/json",
        "text/event-stream; charset=utf-8",
    ),
}


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        # Never print body values or headers; this log is safe to retain locally.
        print("[mock] " + (fmt % args), flush=True)

    def send_bytes(self, body: bytes, content_type: str, status: int = 200, stream: bool = False):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("X-Mock-Upstream", "yes")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if stream:
            for line in body.splitlines(keepends=True):
                self.wfile.write(line)
                self.wfile.flush()
                time.sleep(SSE_DELAY)
        else:
            self.wfile.write(body)
            self.wfile.flush()

    def do_GET(self):
        parsed = urlsplit(self.path)
        if parsed.path == "/v1/models":
            body = (ROOT / "models/response.json").read_bytes()
            self.send_bytes(body, "application/json")
            return
        self.send_bytes(b'{"error":{"message":"mock route not found"}}', "application/json", 404)

    def do_POST(self):
        parsed = urlsplit(self.path)
        length = int(self.headers.get("Content-Length", "0"))
        request_body = self.rfile.read(length)
        try:
            request_json = json.loads(request_body.decode("utf-8"))
            stream = bool(request_json.get("stream"))
        except Exception:
            stream = False
        print(f"[mock] POST {parsed.path} bytes={len(request_body)} stream={stream}", flush=True)
        if parsed.path not in CASES:
            self.send_bytes(b'{"error":{"message":"mock route not found"}}', "application/json", 404)
            return
        json_file, sse_file, json_type, sse_type = CASES[parsed.path]
        if stream:
            self.send_bytes((ROOT / sse_file).read_bytes(), sse_type, stream=True)
        else:
            self.send_bytes((ROOT / json_file).read_bytes(), json_type)


if __name__ == "__main__":
    server = ThreadingHTTPServer(("127.0.0.1", 19091), Handler)
    print("mock upstream listening on http://127.0.0.1:19091", flush=True)
    server.serve_forever()
