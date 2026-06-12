"""Deliberately partial Anthropic-shaped gateway for harness development.

The conformance suite is written before the product (m0-build-plan phase 1).
This stub gives the harness a real HTTP target so SDK wiring, runner
mechanics, and matrix.json reporting can be proven without any Atlas code.
It implements just enough of POST /v1/messages — non-streaming, text only,
plus Anthropic-shaped error envelopes — for group G1 and the error subset
of G7 to pass. Everything else (streaming, tools, /v1/models, the OpenAI
surface) is missing on purpose so the harness demonstrably reports
structured failures.

Behavior model: a "trivially competent model" that replies with the last
double-quoted span found in the system prompt + final user message.
Conformance tests phrase tasks as `Reply with exactly "..."`, which any
real model at temperature 0 also satisfies (suite principle 4: structural
assertions, tasks any competent model passes).
"""

import json
import re
import threading
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

DEFAULT_API_KEY = "conformance-stub-key"
DEFAULT_MODEL = "stub-small"

_FALLBACK_REPLY = "Atlas conformance stub reply."


def _block_text(content) -> str:
    """Extract plain text from a string or content-block-list field."""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return " ".join(
            b.get("text", "")
            for b in content
            if isinstance(b, dict) and b.get("type") == "text"
        )
    return ""


class _Handler(BaseHTTPRequestHandler):
    server_version = "atlas-stub-gateway/0"

    def log_message(self, format, *args):  # noqa: A002 — base class signature
        pass  # keep harness output clean

    def _send_json(self, status: int, payload: dict) -> None:
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _send_error(self, status: int, err_type: str, message: str) -> None:
        self._send_json(status, {"type": "error", "error": {"type": err_type, "message": message}})

    def do_GET(self):  # noqa: N802 — http.server API
        self._send_error(404, "not_found_error", f"stub gateway: no route for GET {self.path}")

    def do_POST(self):  # noqa: N802 — http.server API
        if self.path != "/v1/messages":
            self._send_error(404, "not_found_error", f"stub gateway: no route for POST {self.path}")
            return

        gw: StubGateway = self.server.gateway  # type: ignore[attr-defined]
        if self.headers.get("x-api-key") != gw.api_key:
            self._send_error(401, "authentication_error", "invalid or missing x-api-key header")
            return

        try:
            raw = self.rfile.read(int(self.headers.get("content-length") or 0))
            req = json.loads(raw)
            if not isinstance(req, dict):
                raise ValueError
        except ValueError:
            self._send_error(400, "invalid_request_error", "request body is not a JSON object")
            return

        model = req.get("model")
        if not isinstance(model, str) or not model:
            self._send_error(400, "invalid_request_error", "model: field required")
            return
        if model != gw.model:
            self._send_error(404, "not_found_error", f"model not found: {model}")
            return

        max_tokens = req.get("max_tokens")
        if not isinstance(max_tokens, int) or max_tokens < 1:
            self._send_error(400, "invalid_request_error", "max_tokens: integer >= 1 required")
            return

        messages = req.get("messages")
        if not isinstance(messages, list) or not messages:
            self._send_error(400, "invalid_request_error", "messages: non-empty list required")
            return

        if req.get("stream"):
            self._send_error(
                400,
                "invalid_request_error",
                "streaming is not implemented in the stub gateway (lands in m0-build-plan phase 3)",
            )
            return

        system_text = _block_text(req.get("system", ""))
        last_user = next(
            (
                _block_text(m.get("content"))
                for m in reversed(messages)
                if isinstance(m, dict) and m.get("role") == "user"
            ),
            "",
        )

        # "Generate": echo the last double-quoted span in the instructions.
        spans = re.findall(r'"([^"]*)"', f"{system_text}\n{last_user}")
        reply = spans[-1] if spans and spans[-1] else _FALLBACK_REPLY

        # Generic post-processing, same order a real engine applies it:
        # stop sequences cut generation first, then the token budget.
        stop_reason, stop_sequence = "end_turn", None
        hits = [
            (reply.find(s), s)
            for s in (req.get("stop_sequences") or [])
            if isinstance(s, str) and s and s in reply
        ]
        if hits:
            idx, seq = min(hits)
            reply = reply[:idx]
            stop_reason, stop_sequence = "stop_sequence", seq

        words = reply.split()  # stub tokenization: 1 token == 1 word
        if len(words) > max_tokens:
            reply = " ".join(words[:max_tokens])
            stop_reason, stop_sequence = "max_tokens", None

        input_tokens = max(
            1,
            len(system_text.split())
            + sum(len(_block_text(m.get("content")).split()) for m in messages if isinstance(m, dict)),
        )
        self._send_json(
            200,
            {
                "id": f"msg_stub_{uuid.uuid4().hex[:12]}",
                "type": "message",
                "role": "assistant",
                "model": model,
                "content": [{"type": "text", "text": reply}],
                "stop_reason": stop_reason,
                "stop_sequence": stop_sequence,
                "usage": {"input_tokens": input_tokens, "output_tokens": max(1, len(reply.split()))},
            },
        )


class _Server(ThreadingHTTPServer):
    daemon_threads = True


class StubGateway:
    """In-process stub server; the runner owns its lifecycle."""

    def __init__(self, api_key: str = DEFAULT_API_KEY, model: str = DEFAULT_MODEL):
        self.api_key = api_key
        self.model = model
        self._server: _Server | None = None
        self._thread: threading.Thread | None = None

    @property
    def base_url(self) -> str:
        assert self._server is not None, "gateway not started"
        return f"http://127.0.0.1:{self._server.server_address[1]}"

    def start(self) -> str:
        self._server = _Server(("127.0.0.1", 0), _Handler)
        self._server.gateway = self  # type: ignore[attr-defined]
        self._thread = threading.Thread(target=self._server.serve_forever, name="stub-gateway", daemon=True)
        self._thread.start()
        return self.base_url

    def stop(self) -> None:
        if self._server is not None:
            self._server.shutdown()
            self._server.server_close()
            self._server = None
