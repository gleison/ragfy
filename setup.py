#!/usr/bin/env python3
"""Guided setup for the Claude Code and Codex personal knowledge base."""
from __future__ import annotations

import getpass
import json
import os
import subprocess
import sys
from pathlib import Path

from guided_setup import configure_walkthrough, estimate_summary, platform_name, venv_python


KB_ROOT = Path(__file__).resolve().parent
CONFIG_PATH = KB_ROOT / "config.json"
ENV_PATH = KB_ROOT / ".env"


def save_guided_config(path, cfg):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(cfg, indent=2) + "\n", encoding="utf-8")
    json.loads(path.read_text(encoding="utf-8"))


def load_config():
    if not CONFIG_PATH.exists():
        return {}
    try:
        return json.loads(CONFIG_PATH.read_text(encoding="utf-8"))
    except ValueError as error:
        raise SystemExit(f"config.json is not valid JSON: {error}")


def ask(prompt, default=None):
    suffix = f" [{default}]" if default else ""
    while True:
        value = input(f"{prompt}{suffix}: ").strip()
        if value:
            return value
        if default is not None:
            return default
        print("Please enter a value.")


def confirm(prompt, default=True):
    hint = "Y/n" if default else "y/N"
    while True:
        value = input(f"{prompt} [{hint}]: ").strip().lower()
        if not value:
            return default
        if value in {"y", "yes"}:
            return True
        if value in {"n", "no"}:
            return False
        print("Please answer y or n.")


def require_typed_yes(prompt):
    return input(f"{prompt}\nType 'yes' to continue: ").strip().lower() == "yes"


class TerminalIO:
    def ask(self, prompt, default=None):
        return ask(prompt, default)

    def tell(self, message):
        print(message)


def _write_env(key, value):
    lines = []
    found = False
    if ENV_PATH.exists():
        for line in ENV_PATH.read_text(encoding="utf-8").splitlines():
            if line.startswith(f"{key}="):
                lines.append(f"{key}={value}")
                found = True
            else:
                lines.append(line)
    if not found:
        lines.append(f"{key}={value}")
    ENV_PATH.write_text("\n".join(lines) + "\n", encoding="utf-8")
    if os.name != "nt":
        os.chmod(ENV_PATH, 0o600)


def ensure_environment():
    platform = platform_name()
    python = venv_python(KB_ROOT, platform)
    if not python.exists():
        print("Creating a private Python environment...")
        subprocess.run([sys.executable, "-m", "venv", str(KB_ROOT / ".venv")], check=True)
    if Path(sys.prefix).resolve() != (KB_ROOT / ".venv").resolve():
        os.execv(str(python), [str(python), str(Path(__file__).resolve()), *sys.argv[1:]])
    subprocess.run([sys.executable, "-m", "pip", "install", "-q", "-r", str(KB_ROOT / "requirements.txt")], check=True)


def _set_secret(name, prompt):
    value = os.environ.get(name, "").strip()
    if value:
        return value
    value = getpass.getpass(f"{prompt} (hidden): ").strip()
    if not value:
        raise SystemExit(f"{name} is required for this choice.")
    _write_env(name, value)
    os.environ[name] = value
    return value


def _configure_paid_embedding(cfg):
    if cfg["embedding"]["provider"] != "local":
        _set_secret("OPENAI_API_KEY", "Paste your OpenAI-compatible embedding API key")
def _read_sources(cfg):
    from ingest import CONNECTORS

    docs = []
    for source in cfg.get("sources", []):
        reader = CONNECTORS.get(source)
        if reader is None:
            continue
        print(f"Reading {source}...")
        try:
            docs.extend(reader())
        except Exception as error:
            print(f"  {source}: skipped ({str(error)[:160]})")
    return docs


def _ingest(docs):
    import chunking
    import store

    db = store.connect()
    pieces = [piece for doc in docs for piece in chunking.chunk_doc(doc)]
    changed = sum(
        store.upsert(db, piece["source"], piece["source_id"], piece.get("ts"), piece.get("title"),
                     piece.get("content"), piece.get("metadata"), piece.get("kind"), piece.get("area"))
        for piece in pieces
    )
    db.commit()
    print(f"Wrote {changed} new or changed chunk(s). Creating embeddings...")
    embedded = store.embed_missing(db)
    print(f"Embedded {embedded} chunk(s).")
    return len(pieces)


def _install_integrations(cfg):
    from platform_setup import hook_paths, install_schedule, merge_hook_file, recall_command, schedule_definition

    platform = cfg["onboarding"]["platform"]
    if confirm("Install automatic recall hooks for Claude Code and Codex?", True):
        command = recall_command(KB_ROOT, platform)
        for path in hook_paths(Path.home(), platform):
            merge_hook_file(path, command)
            print(f"Updated {path}")
        print("Start a new Claude Code or Codex session. You should then see KB RECALL when a prompt matches your library.")
    schedule = cfg.get("schedule", {})
    if schedule.get("enabled") and confirm("Install the refresh schedule now?", True):
        (KB_ROOT / "logs").mkdir(exist_ok=True)
        definition = schedule_definition(platform, KB_ROOT, schedule["time"],
                                         schedule["frequency"], schedule.get("weekday", "monday"))
        install_schedule(definition, lambda command: subprocess.run(command, check=True))
        print("The refresh schedule is installed. It runs only while this computer is awake.")


def main():
    ensure_environment()
    cfg = configure_walkthrough(load_config(), TerminalIO())
    save_guided_config(CONFIG_PATH, cfg)
    _configure_paid_embedding(cfg)
    history = cfg["onboarding"]["history_mode"]
    if history == "now":
        print("Saved your choices. No past material was ingested.")
        _install_integrations(cfg)
        return
    docs = _read_sources(cfg)
    import chunking
    pieces = [piece for doc in docs for piece in chunking.chunk_doc(doc)]
    tokens = sum(max(1, len((piece.get("title") or "") + (piece.get("content") or "")) // 4) for piece in pieces)
    estimate = estimate_summary(tokens, len(docs), cfg["embedding"])
    print(f"Found {estimate['records']} source file(s), {len(pieces)} chunk(s), and about {estimate['tokens']:,} tokens.")
    print(f"Estimated embedding time: {estimate['estimated_minutes']} minute(s).")
    print(f"Estimated embedding cost: ${estimate['estimated_cost_usd']:.4f}.")
    if not docs:
        print("No readable material was found. Your choices are saved; add a source and run setup.py again.")
        return
    if history == "sample":
        docs = docs[: min(25, len(docs))]
        print(f"The walkthrough will ingest a sample of {len(docs)} source file(s) first.")
    approved = require_typed_yes("This sends text to a paid embedding API.") if estimate["estimated_cost_usd"] else confirm("Ingest this material now?", True)
    if not approved:
        print("Nothing was ingested. Your choices are saved for the next run.")
        return
    _ingest(docs)
    _install_integrations(cfg)
    print("Use the hook for a few real prompts, then run `python chunk_review.py --hours 24`. Mark the exact injected chunks Useful or Junk. After three reviewed prompts and eight reviewed chunks, saving the review automatically chooses the smallest relevant chunk limit.")


if __name__ == "__main__":
    main()
