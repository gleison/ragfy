"""Question flow and platform-neutral configuration for the KB walkthrough."""
from __future__ import annotations

import platform as _platform
from dataclasses import dataclass, field
from pathlib import Path


LOCAL_MODEL = "BAAI/bge-small-en-v1.5"
OPENAI_PRICE_PER_MILLION = 0.02
LOCAL_TOKENS_PER_MINUTE = 25_000
REMOTE_TOKENS_PER_MINUTE = 250_000
SUPPORTED_SOURCES = ("documents", "obsidian", "youtube", "transcripts", "codex_sessions")


def platform_name(system=None):
    name = (system or _platform.system()).lower()
    if name.startswith("win"):
        return "windows"
    if name == "darwin":
        return "macos"
    return "linux"


def venv_python(root, platform=None):
    root = Path(root)
    if platform == "windows":
        return root / ".venv" / "Scripts" / "python.exe"
    return root / ".venv" / "bin" / "python"


@dataclass
class ScriptedIO:
    """Small terminal facade that lets the setup flow run under unit tests."""

    answers: list[str] = field(default_factory=list)
    output: list[str] = field(default_factory=list)

    def ask(self, prompt, default=None):
        self.output.append(prompt)
        answer = self.answers.pop(0).strip() if self.answers else ""
        return answer or default or ""

    def tell(self, message):
        self.output.append(message)


def _choice(io, prompt, choices, default):
    response = io.ask(f"{prompt} ({'/'.join(choices)})", default).lower()
    return response if response in choices else default


def _source_config(source, io):
    if source == "documents":
        path = io.ask("Folder to include")
        return {"documents": {"roots": [{"area": "documents", "path": path, "glob": "**/*"}]}}
    if source == "obsidian":
        path = io.ask("Obsidian vault path")
        return {"obsidian": {"vaults": [{"path": path}], "default_area": "notes"}}
    if source == "youtube":
        raw = io.ask("YouTube video URLs, separated by commas")
        urls = [url.strip() for url in raw.split(",") if url.strip()]
        return {"youtube": {"urls": urls}}
    return {source: {"enabled": True}}


def _merge(cfg, update):
    for key, value in update.items():
        if isinstance(value, dict) and isinstance(cfg.get(key), dict):
            cfg[key].update(value)
        else:
            cfg[key] = value


def configure_walkthrough(cfg, io, platform=None):
    """Ask the public setup questions and return a JSON-serializable config."""
    cfg = dict(cfg)
    platform = platform or platform_name()
    io.tell(f"Detected platform: {platform}.")
    cfg["purpose"] = io.ask("What do you want this library to help you recall?", cfg.get("purpose"))

    cfg["vector_backend"] = "sqlite"
    io.tell("Search stays local in a SQLite file on this computer.")

    embedding_choice = _choice(io, "Use free local embeddings or a paid API?", ("free", "paid"), "free")
    cfg["embedding"] = (
        {"provider": "local", "model": LOCAL_MODEL}
        if embedding_choice == "free"
        else {"provider": "openai", "model": "text-embedding-3-small", "api_key_env": "OPENAI_API_KEY"}
    )
    cfg["privacy"] = {"local_only": embedding_choice == "free"}

    raw_sources = io.ask(
        "Sources to include: documents, obsidian, youtube, transcripts, codex_sessions (comma-separated; blank for none)",
        "",
    )
    sources = [source.strip().lower() for source in raw_sources.split(",") if source.strip()]
    invalid = sorted(set(sources) - set(SUPPORTED_SOURCES))
    if invalid:
        raise ValueError("Unsupported source(s): " + ", ".join(invalid))
    cfg["sources"] = list(dict.fromkeys(sources))
    for source in cfg["sources"]:
        _merge(cfg, _source_config(source, io))

    history_mode = _choice(io, "Import old material, test a sample, or start now?", ("backfill", "sample", "now"), "sample")
    schedule_choice = _choice(io, "Refresh schedule: daily, weekly, or none?", ("daily", "weekly", "none"), "none")
    schedule = {"enabled": schedule_choice != "none", "frequency": schedule_choice}
    if schedule["enabled"]:
        schedule["time"] = io.ask("What time should it refresh? Use 24-hour HH:MM", "03:00")
        if schedule_choice == "weekly":
            schedule["weekday"] = _choice(
                io, "Which day should it refresh?",
                ("monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"),
                "monday",
            )
        io.tell("A local refresh runs only while this computer is awake.")
    cfg["onboarding"] = {"platform": platform, "history_mode": history_mode}
    cfg["schedule"] = schedule
    return cfg


def estimate_summary(tokens, records, embedding):
    provider = embedding.get("provider", "local")
    rate = REMOTE_TOKENS_PER_MINUTE if provider != "local" else LOCAL_TOKENS_PER_MINUTE
    return {
        "records": int(records),
        "tokens": int(tokens),
        "estimated_minutes": max(1, round(tokens / rate, 1)),
        "estimated_cost_usd": round(tokens / 1_000_000 * OPENAI_PRICE_PER_MILLION, 4)
        if provider != "local" else 0,
    }
