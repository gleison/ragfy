"""Generic document connector — folders of PDFs, text, and markdown.

Configured entirely in config.json, so a new body of material is a config entry
rather than a new connector:

  "documents": {
    "roots": [
      {"area": "research", "path": "~/Documents/Research", "glob": "**/*.pdf"},
      {"area": "finance", "path": "~/Documents/Statements"}
    ],
    "max_file_mb": 40
  }

`area` is the life-area namespace. It is stored on every row and can be passed
to store.search(area=...) so one realm's material never answers another realm's
question. Chunking happens in ingest.py, which every connector goes through, so
a 300-page PDF arrives as passages rather than one blurred vector.

PDF text extraction needs pypdf (`pip install pypdf`). Without it the connector
reports what it skipped instead of failing the run: a missing optional
dependency should never take the nightly ingest down.
"""
import hashlib
import sys
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent.parent))
import config

TEXT_EXT = {".txt", ".md", ".markdown", ".rst", ".org"}
PDF_EXT = {".pdf"}
DEFAULT_GLOB = "**/*"
MIN_CHARS = 200


def _read_pdf(path, max_pages=400):
    try:
        from pypdf import PdfReader
    except ImportError:
        return None, "pypdf not installed (pip install pypdf)"
    try:
        reader = PdfReader(str(path))
        pages = reader.pages[:max_pages]
        text = "\n\n".join((p.extract_text() or "") for p in pages)
        return text, None
    except Exception as e:
        return None, str(e)[:100]


def _read_text(path):
    try:
        return path.read_text(encoding="utf-8", errors="replace"), None
    except Exception as e:
        return None, str(e)[:100]


def fetch(roots=None):
    cfg = config.load().get("documents", {})
    roots = roots or cfg.get("roots", [])
    max_mb = float(cfg.get("max_file_mb", 40))
    if not roots:
        print("  documents: no roots configured")
        return []

    docs = []
    missing_dep = skipped = 0
    for root in roots:
        base = Path(root["path"]).expanduser()
        area = root.get("area") or base.name
        pattern = root.get("glob", DEFAULT_GLOB)
        if not base.exists():
            print(f"  documents[{area}]: {base} does not exist")
            continue
        found = 0
        for path in sorted(base.glob(pattern)):
            if not path.is_file():
                continue
            ext = path.suffix.lower()
            if ext not in TEXT_EXT and ext not in PDF_EXT:
                continue
            if path.stat().st_size > max_mb * 1024 * 1024:
                skipped += 1
                continue
            text, err = (_read_pdf(path) if ext in PDF_EXT else _read_text(path))
            if text is None:
                if err and "pypdf" in err:
                    missing_dep += 1
                else:
                    skipped += 1
                continue
            text = text.strip()
            if len(text) < MIN_CHARS:
                skipped += 1
                continue
            # Path-based id, hashed so a long path does not become the key.
            sid = f"{area}:{hashlib.sha1(str(path).encode()).hexdigest()[:16]}"
            ts = datetime.fromtimestamp(path.stat().st_mtime, tz=timezone.utc).isoformat()
            docs.append(dict(
                source="documents", source_id=sid, ts=ts,
                title=path.stem.replace("_", " ").replace("-", " ")[:160],
                content=text, kind="document", area=area,
                metadata={"path": str(path), "area": area, "ext": ext,
                          "bytes": path.stat().st_size},
            ))
            found += 1
        print(f"  documents[{area}]: {found} file(s)")
    if missing_dep:
        print(f"  documents: {missing_dep} PDF(s) skipped — run `pip install pypdf`")
    if skipped:
        print(f"  documents: {skipped} file(s) skipped (too large, empty, or unreadable)")
    return docs
