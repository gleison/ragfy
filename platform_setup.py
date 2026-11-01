"""Native hook and refresh-schedule configuration for supported platforms."""
from __future__ import annotations

import json
from pathlib import Path

from guided_setup import venv_python


def recall_command(root, platform):
    python = venv_python(root, platform)
    recall = Path(root) / "recall.py"
    if platform == "windows":
        return f'"{python}" "{recall}"'
    return f'"{python}" "{recall}"'


def merge_hook_file(path, command):
    path = Path(path)
    current = {}
    if path.exists():
        current = json.loads(path.read_text(encoding="utf-8"))
        backup = path.with_suffix(path.suffix + ".bak")
        backup.write_text(path.read_text(encoding="utf-8"), encoding="utf-8")
    hooks = current.setdefault("hooks", {})
    entries = hooks.setdefault("UserPromptSubmit", [])
    exists = any(
        hook.get("command") == command
        for entry in entries if isinstance(entry, dict)
        for hook in entry.get("hooks", []) if isinstance(hook, dict)
    )
    if not exists:
        entries.append({"hooks": [{"type": "command", "command": command, "timeout": 12}]})
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(current, indent=2) + "\n", encoding="utf-8")
    json.loads(path.read_text(encoding="utf-8"))
    return current


def hook_paths(home, platform):
    home = Path(home)
    if platform == "windows":
        return [home / ".claude" / "settings.json", home / ".codex" / "hooks.json"]
    return [home / ".claude" / "settings.json", home / ".codex" / "hooks.json"]


def schedule_definition(platform, root, when, frequency="daily", weekday="monday"):
    root = Path(root)
    hour, minute = (int(part) for part in when.split(":", 1))
    frequency = frequency.lower()
    if frequency not in {"daily", "weekly"}:
        raise ValueError("frequency must be daily or weekly")
    weekdays = {"sunday": ("SUN", 0), "monday": ("MON", 1), "tuesday": ("TUE", 2),
                "wednesday": ("WED", 3), "thursday": ("THU", 4), "friday": ("FRI", 5),
                "saturday": ("SAT", 6)}
    if weekday.lower() not in weekdays:
        raise ValueError("weekday must be monday through sunday")
    windows_day, launchd_day = weekdays[weekday.lower()]
    python = venv_python(root, platform)
    ingest = root / "ingest.py"
    log = root / "logs" / "refresh.log"
    if platform == "windows":
        python, ingest, log = (str(value).replace("/", "\\") for value in (python, ingest, log))
        task_name = "PersonalKnowledgeBaseRefresh"
        action = f'"{python}" "{ingest}" >> "{log}" 2>&1'
        arguments = ["/Create", "/F", "/SC", frequency.upper(), "/TN", task_name,
                     "/TR", action, "/ST", when]
        if frequency == "weekly":
            arguments.extend(["/D", windows_day])
        return {
            "kind": "schtasks",
            "command": "schtasks",
            "arguments": arguments,
            "task_name": task_name,
        }
    label = "com.personal-knowledge-base.refresh"
    plist = f"""<?xml version=\"1.0\" encoding=\"UTF-8\"?>
<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">
<plist version=\"1.0\"><dict>
<key>Label</key><string>{label}</string>
<key>ProgramArguments</key><array><string>{python}</string><string>{ingest}</string></array>
<key>StartCalendarInterval</key><dict><key>Hour</key><integer>{hour}</integer><key>Minute</key><integer>{minute}</integer>{f'<key>Weekday</key><integer>{launchd_day}</integer>' if frequency == 'weekly' else ''}</dict>
<key>StandardOutPath</key><string>{log}</string><key>StandardErrorPath</key><string>{log}</string>
</dict></plist>"""
    return {"kind": "launchd", "path": Path.home() / "Library" / "LaunchAgents" / f"{label}.plist", "content": plist}


def install_schedule(definition, runner):
    if definition["kind"] == "schtasks":
        return runner([definition["command"], *definition["arguments"]])
    path = definition["path"]
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(definition["content"], encoding="utf-8")
    return runner(["launchctl", "load", str(path)])
