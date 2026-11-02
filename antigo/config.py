"""Loads config.json, and the environment it needs.

Importing this also loads variables from a local .env, so every entry point
works without anyone remembering to `source` anything first. Real environment
variables win over the file.

A missing config.json is a normal state, not an error: it is what a fresh
install looks like before setup runs. Falling back to config.example.json means
`--help`, imports, and diagnostics all still work before anything is configured,
instead of every command dying with the same message.
"""
import json
import os
from pathlib import Path

HERE = Path(__file__).parent
CONFIG_PATH = HERE / "config.json"
EXAMPLE_PATH = HERE / "config.example.json"

_warned = False


def _load_env():
    for envfile in (HERE / ".env", Path.home() / ".kb.env"):
        if not envfile.exists():
            continue
        for line in envfile.read_text().splitlines():
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            k, v = line.split("=", 1)
            v = v.strip().strip('"').strip("'")
            if v:
                os.environ.setdefault(k.strip(), v)


_load_env()


def configured():
    """True once setup has written a real config."""
    return CONFIG_PATH.exists()


def load():
    global _warned
    if CONFIG_PATH.exists():
        try:
            return json.loads(CONFIG_PATH.read_text())
        except ValueError as e:
            raise SystemExit(f"config.json is not valid JSON: {e}")
    if EXAMPLE_PATH.exists():
        if not _warned:
            print("  (no config.json yet, using config.example.json defaults. "
                  "Run setup.py, or ask Claude to read INSTALL.md.)")
            _warned = True
        return json.loads(EXAMPLE_PATH.read_text())
    return {}
