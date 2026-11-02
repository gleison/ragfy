"""Read local Codex history when the user chooses it as a source."""
import hashlib
import json
from datetime import datetime, timezone
from pathlib import Path

import config


def _text_from(item):
    if isinstance(item, str):
        return item.strip()
    if not isinstance(item, dict):
        return ""
    for key in ("text", "content", "message", "prompt"):
        value = item.get(key)
        if isinstance(value, str) and value.strip():
            return value.strip()
    return ""


def fetch(paths=None):
    cfg = config.load().get("codex_sessions", {})
    candidates = paths if paths is not None else cfg.get("paths") or [Path.home() / ".codex" / "history.jsonl"]
    docs = []
    for raw_path in candidates:
        path = Path(raw_path).expanduser()
        if not path.exists():
            print(f"  codex_sessions: {path} does not exist")
            continue
        for line_number, raw in enumerate(path.read_text(encoding="utf-8", errors="replace").splitlines(), 1):
            try:
                text = _text_from(json.loads(raw))
            except ValueError:
                continue
            if len(text) < 120:
                continue
            docs.append({
                "source": "codex_sessions",
                "source_id": hashlib.sha1(f"{path}:{line_number}:{text}".encode()).hexdigest()[:16],
                "ts": datetime.fromtimestamp(path.stat().st_mtime, tz=timezone.utc).isoformat(),
                "title": f"Codex session entry {line_number}",
                "content": text,
                "kind": "session",
                "area": "sessions",
                "metadata": {"path": str(path), "line": line_number},
            })
    print(f"  codex_sessions: {len(docs)} entry(s)")
    return docs
