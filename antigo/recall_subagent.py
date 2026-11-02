#!/usr/bin/env python3
"""SubagentStart hook — gives a spawned subagent the same recall the main agent gets.

`UserPromptSubmit` only fires on a human-submitted prompt, so subagents used to
start with none of Mason's knowledge base. `SubagentStart` fires when the Agent
tool spawns one, and whatever this returns as `additionalContext` is placed in
that subagent's context before its first prompt.

Output shape differs from recall.py: this hook speaks JSON on stdout rather than
plain text. It must never block a spawn, so every failure path exits 0 silently.
The first payload seen is written to logs/subagent_payload.json so the field
names can be confirmed from a real event instead of assumed.
"""
import json
import os
import sys
from pathlib import Path

HERE = Path(__file__).parent
sys.path.insert(0, str(HERE))

SNIPPET = 220
PROMPT_FIELDS = ("prompt", "description", "task", "instructions", "user_prompt")


AGENT_TOOLS = {"Agent", "Task"}


def _from_transcript(path, agent_id=None):
    """Recover the subagent's prompt from the parent transcript.

    Confirmed from a live payload: SubagentStart carries only agent_id,
    agent_type, cwd, hook_event_name, prompt_id, session_id and
    transcript_path. There is no prompt field, which is why this hook produced
    nothing at all until now. The prompt does exist in the parent's transcript,
    as the input to the Agent tool_use that is spawning this subagent, so read
    it from the tail of that file.
    """
    try:
        lines = Path(path).read_text(encoding="utf-8", errors="replace").splitlines()
    except Exception:
        return None
    best = None
    for raw in lines[-400:]:                 # the spawn is always near the end
        try:
            d = json.loads(raw)
        except ValueError:
            continue
        content = (d.get("message") or {}).get("content")
        if not isinstance(content, list):
            continue
        for block in content:
            if not isinstance(block, dict) or block.get("type") != "tool_use":
                continue
            if block.get("name") not in AGENT_TOOLS:
                continue
            prompt = (block.get("input") or {}).get("prompt")
            if isinstance(prompt, str) and len(prompt.strip()) >= 12:
                best = prompt.strip()        # keep the latest one seen
    return best


def _find_prompt(payload):
    for key in PROMPT_FIELDS:
        v = payload.get(key)
        if isinstance(v, str) and len(v.strip()) >= 12:
            return v.strip()
    ti = payload.get("tool_input") or {}
    if isinstance(ti, dict):
        for key in PROMPT_FIELDS:
            v = ti.get(key)
            if isinstance(v, str) and len(v.strip()) >= 12:
                return v.strip()
    tp = payload.get("transcript_path")
    if tp:
        return _from_transcript(tp, payload.get("agent_id"))
    return None


def main():
    if os.environ.get("CLAUDE_KB_DISTILL"):
        return
    try:
        payload = json.load(sys.stdin)
    except ValueError:
        return

    # Append every invocation, so "did the hook fire?" is answerable from
    # evidence instead of inferred from whether a subagent noticed its context.
    try:
        from datetime import datetime, timezone
        log = HERE / "logs" / "subagent_hook.log"
        log.parent.mkdir(exist_ok=True)
        with log.open("a") as fh:
            fh.write(f"{datetime.now(timezone.utc).isoformat()} FIRED "
                     f"keys={sorted(payload)} "
                     f"{json.dumps(payload)[:600]}\n")
    except Exception:
        pass

    prompt = _find_prompt(payload)
    if not prompt:
        return

    import config  # noqa: F401 — loads .env
    import store

    limit = int(config.load().get("retrieval", {}).get("subagent_limit", 4))
    db = store.connect()
    try:
        results = store.search(db, prompt, limit=limit, log=True,
                               session=payload.get("session_id"),
                               agent=payload.get("agent_type") or "subagent")
    except Exception:
        return
    if not results:
        return

    lines = [f"=== KB RECALL ({len(results)} match"
             f"{'' if len(results) == 1 else 'es'} from Mason's knowledge base) ==="]
    for _, row in results:
        snippet = " ".join((row["content"] or "").split())[:SNIPPET]
        kind = row["kind"] or row["source"]
        lines.append(f"[{kind} {(row['ts'] or '')[:10]}] {row['title']} — {snippet}")

    print(json.dumps({"hookSpecificOutput": {
        "hookEventName": "SubagentStart",
        "additionalContext": "\n".join(lines),
    }}))


if __name__ == "__main__":
    try:
        main()
    except Exception:
        pass
    sys.exit(0)
