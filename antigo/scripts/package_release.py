"""Build a clean distributable archive from this source directory."""
from __future__ import annotations

import argparse
import zipfile
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent
PACKAGE = ROOT.name
EXCLUDED_PARTS = {".venv", "__pycache__", "tests", ".git", ".superpowers"}
EXCLUDED_PREFIXES = {"docs/superpowers"}
EXCLUDED_NAMES = {".DS_Store", ".env", "config.json", "kb.sqlite", "probe.html", "results.html", "calibration-report.html"}


def _include(path):
    rel = path.relative_to(ROOT)
    return (
        path.is_file()
        and not any(part in EXCLUDED_PARTS for part in rel.parts)
        and not any(str(rel).startswith(prefix) for prefix in EXCLUDED_PREFIXES)
        and path.name not in EXCLUDED_NAMES
        and path.suffix != ".zip"
    )


def package_release(destination):
    destination = Path(destination)
    destination.parent.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(destination, "w", zipfile.ZIP_DEFLATED) as archive:
        for path in sorted(ROOT.rglob("*")):
            if _include(path):
                archive.write(path, Path(PACKAGE) / path.relative_to(ROOT))
    return destination


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Build a clean installer archive.")
    parser.add_argument("destination", nargs="?", type=Path,
                        default=ROOT.parent / f"{PACKAGE}.zip")
    args = parser.parse_args()
    print(package_release(args.destination))
