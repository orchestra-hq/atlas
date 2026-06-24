#!/usr/bin/env python3
"""engine-raw.py — decode ATLAS_DEBUG_ENGINE_RAW captures from an Atlas engine log.

When ATLAS_DEBUG_ENGINE_RAW is set, the gateway logs each raw upstream engine
response as an slog line ending in `body="<Go-quoted JSON>"`. Eyeballing those
by hand means writing a throwaway python one-liner every time (which then prompts
for approval, because `python3 -c` is arbitrary code execution and can't be
allowlisted). This is that one-liner, made permanent and allowlistable.

    python3 scripts/engine-raw.py <logfile>          # one summary line per capture
    python3 scripts/engine-raw.py <logfile> --full   # + pretty-printed JSON body

The summary surfaces what the vLLM/G4 thinking diagnosis actually cares about:
whether each response carried reasoning_content / reasoning / content, and the
finish_reason — so a regression shows up as a column going empty.
"""

import argparse
import json
import re
import sys

MARKER = "ATLAS_DEBUG_ENGINE_RAW"
# slog text format puts the body last: ... status=200 body="<Go-quoted JSON>"
BODY_RE = re.compile(r'body=(.*)$')
KV_RE = re.compile(r'(\bengine|\bmodel|\bstatus)=(\S+)')


def decode_body(raw: str) -> str:
    """Unquote a Go-/slog-quoted string into the JSON it wraps."""
    raw = raw.strip()
    # The common case: it's a JSON-compatible quoted string ("\"...\"").
    try:
        return json.loads(raw)
    except json.JSONDecodeError:
        # Fallback for Go-quoting that isn't valid JSON: strip the outer quotes
        # and let Python's unicode_escape undo the backslash escapes.
        if len(raw) >= 2 and raw[0] == '"' and raw[-1] == '"':
            return raw[1:-1].encode("utf-8").decode("unicode_escape")
        return raw


def summarize(obj: dict) -> str:
    try:
        msg = obj["choices"][0]["message"]
    except (KeyError, IndexError, TypeError):
        return "  (no choices[0].message)"
    finish = (obj.get("choices") or [{}])[0].get("finish_reason")

    def field(name):
        v = msg.get(name)
        if v is None:
            return f"{name}=-"
        if isinstance(v, str):
            return f"{name}={len(v)}c" if v else f"{name}=empty"
        return f"{name}=set"

    return (
        f"  finish={finish}  "
        + "  ".join(field(n) for n in ("reasoning_content", "reasoning", "content"))
    )


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("logfile")
    ap.add_argument("--full", action="store_true", help="pretty-print each decoded JSON body")
    args = ap.parse_args()

    try:
        lines = [ln for ln in open(args.logfile) if MARKER in ln]
    except OSError as e:
        print(f"engine-raw: {e}", file=sys.stderr)
        return 2

    print(f"{len(lines)} raw capture(s) in {args.logfile}\n")
    for i, ln in enumerate(lines, 1):
        kv = dict((k.strip(), v) for k, v in KV_RE.findall(ln))
        m = BODY_RE.search(ln)
        ctx = f"engine={kv.get('engine', '?')} model={kv.get('model', '?')} status={kv.get('status', '?')}"
        print(f"--- capture {i} ({ctx}) ---")
        if not m:
            print("  (no body= field)")
            continue
        try:
            obj = json.loads(decode_body(m.group(1)))
        except (json.JSONDecodeError, ValueError) as e:
            print(f"  (parse error: {e})")
            continue
        print(summarize(obj))
        if args.full:
            print(json.dumps(obj, indent=2, ensure_ascii=False))
        print()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
