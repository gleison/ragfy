"""Obsidian vault connector.

A vault is a folder of markdown, so the generic documents connector would read
it. It would read it badly, and the difference is worth a dedicated connector:

  Frontmatter. YAML at the top of a note is metadata, not prose. Embedded as
  content it adds `tags: [x, y]` and `aliases:` to the vector of every note,
  which is noise shared by every note in the vault and therefore actively
  harmful: it pulls unrelated notes closer together.

  Daily notes. A note called 2026-08-21.md is about that day. Taking the file's
  modification time instead, as a generic reader must, dates it to whenever the
  file was last touched, which breaks every "what did I do last Tuesday"
  question. The filename is the better source of truth and it is free.

  Folders as areas. Vaults are already organised into top-level folders, and
  those folders are almost always the realms of life this system wants. Using
  them means the areas configure themselves.

  Wikilinks. `[[Composite protocol]]` carries a real concept name. The brackets
  are syntax, so they are stripped, but the words inside stay.

Excluded by default: `.obsidian/` (application settings), `.trash/`, and any
configured templates folder, none of which are knowledge.
"""
import hashlib
import re
import sys
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent.parent))
import config

SKIP_DIRS = {".obsidian", ".trash", ".git", "node_modules", ".smart-env"}
DATE_NAME = re.compile(r"(20\d{2})[-_.]?(\d{2})[-_.]?(\d{2})")
FRONTMATTER = re.compile(r"\A---\s*\n(.*?)\n---\s*\n", re.S)
WIKILINK = re.compile(r"\[\[([^\]|]+)(?:\|([^\]]+))?\]\]")
EMBED = re.compile(r"!\[\[([^\]]+)\]\]")
INLINE_TAG = re.compile(r"(?:^|\s)#([A-Za-z][\w/-]{1,40})")
MD_LINK = re.compile(r"\[([^\]]+)\]\([^)]+\)")
MIN_CHARS = 120


def _parse_frontmatter(text):
    """Return (metadata dict, body). Deliberately a small parser rather than a
    YAML dependency: vault frontmatter is overwhelmingly flat key/value and
    simple lists, and a hard dependency for that is not worth it."""
    m = FRONTMATTER.match(text)
    if not m:
        return {}, text
    meta, key = {}, None
    for raw in m.group(1).splitlines():
        if not raw.strip():
            continue
        if raw.lstrip().startswith("- ") and key:
            meta.setdefault(key, [])
            if isinstance(meta[key], list):
                meta[key].append(raw.lstrip()[2:].strip().strip("\"'"))
            continue
        if ":" in raw:
            key, _, val = raw.partition(":")
            key, val = key.strip(), val.strip()
            if val.startswith("[") and val.endswith("]"):
                meta[key] = [v.strip().strip("\"'") for v in val[1:-1].split(",") if v.strip()]
            elif val:
                meta[key] = val.strip("\"'")
            else:
                meta[key] = []
    return meta, text[m.end():]


def _clean(body):
    body = EMBED.sub(lambda m: f"({m.group(1)})", body)          # ![[image.png]]
    body = WIKILINK.sub(lambda m: m.group(2) or m.group(1), body)  # [[a|b]] -> b
    body = MD_LINK.sub(r"\1", body)                               # [text](url) -> text
    return body.strip()


def _date_from_name(stem):
    m = DATE_NAME.search(stem)
    if not m:
        return None
    try:
        return datetime(int(m.group(1)), int(m.group(2)), int(m.group(3)),
                        tzinfo=timezone.utc)
    except ValueError:
        return None


def _area_for(path, vault, meta, default_area):
    """Frontmatter wins, then the top-level folder, then the configured default."""
    for key in ("area", "realm", "category"):
        v = meta.get(key)
        if isinstance(v, str) and v.strip():
            return v.strip()
        if isinstance(v, list) and v:
            return str(v[0])
    rel = path.relative_to(vault).parts
    if len(rel) > 1:
        return rel[0]
    return default_area


def fetch(vaults=None):
    cfg = config.load().get("obsidian", {})
    vaults = vaults or cfg.get("vaults", [])
    default_area = cfg.get("default_area", "notes")
    skip = SKIP_DIRS | set(cfg.get("skip_folders", []))
    max_mb = float(cfg.get("max_file_mb", 5))
    if not vaults:
        print("  obsidian: no vaults configured")
        return []

    docs, skipped = [], 0
    for entry in vaults:
        if isinstance(entry, str):
            entry = {"path": entry}
        vault = Path(entry["path"]).expanduser()
        if not vault.exists():
            print(f"  obsidian: {vault} does not exist")
            continue
        base_area = entry.get("area") or default_area
        found = 0
        for path in sorted(vault.rglob("*.md")):
            if any(part in skip for part in path.relative_to(vault).parts):
                continue
            if path.stat().st_size > max_mb * 1024 * 1024:
                skipped += 1
                continue
            try:
                raw = path.read_text(encoding="utf-8", errors="replace")
            except Exception:
                skipped += 1
                continue
            meta, body = _parse_frontmatter(raw)
            body = _clean(body)
            if len(body) < MIN_CHARS:
                skipped += 1
                continue
            tags = list(meta.get("tags") or [])
            if isinstance(tags, str):
                tags = [tags]
            tags += INLINE_TAG.findall(body)
            dated = _date_from_name(path.stem)
            ts = (dated or datetime.fromtimestamp(path.stat().st_mtime, tz=timezone.utc)).isoformat()
            if dated and not meta.get("title"):
                # "2026 08 19" matches nothing a person types. Writing the date
                # out in words is what makes "what did I do last Wednesday"
                # reach this note, the same way calendar events are phrased.
                title = f"{dated:%A, %B %-d, %Y}"
            else:
                title = str(meta.get("title") or path.stem).replace("-", " ").replace("_", " ")
            area = entry.get("area") or _area_for(path, vault, meta, base_area)
            if dated:
                body = f"Daily note for {dated:%A, %B %-d, %Y}.\n\n{body}"
            docs.append(dict(
                source="obsidian",
                source_id=hashlib.sha1(str(path).encode()).hexdigest()[:16],
                ts=ts, title=title[:160], content=body,
                kind="daily-note" if dated else "note", area=area,
                metadata={"path": str(path), "vault": vault.name,
                          "tags": sorted(set(tags))[:20],
                          "folder": "/".join(path.relative_to(vault).parts[:-1]),
                          "dated_from_filename": bool(dated)},
            ))
            found += 1
        print(f"  obsidian[{vault.name}]: {found} note(s)")
    if skipped:
        print(f"  obsidian: {skipped} file(s) skipped (empty, oversized, or unreadable)")
    return docs
