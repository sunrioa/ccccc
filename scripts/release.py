#!/usr/bin/env python3
"""Build portable release archives using Go (no local session data is included)."""
import argparse
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import zipfile

ROOT = Path(__file__).resolve().parents[1]
TARGETS = [(os_name, arch) for os_name in ("darwin", "windows", "linux") for arch in ("amd64", "arm64")]

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--out", type=Path, default=ROOT / "dist")
    args = parser.parse_args()
    dest = args.out.resolve()
    dest.mkdir(parents=True, exist_ok=True)
    sums = []
    for os_name, arch in TARGETS:
        name = f"session-relay-0.2.0-{os_name}-{arch}"
        with tempfile.TemporaryDirectory(prefix="session-relay-build-") as temp:
            package = Path(temp) / name
            package.mkdir()
            binary = package / ("relay.exe" if os_name == "windows" else "relay")
            env = dict(os.environ, GOOS=os_name, GOARCH=arch, CGO_ENABLED="0")
            subprocess.run(["go", "build", "-trimpath", "-ldflags=-s -w", "-o", str(binary), "./cmd/relay"], cwd=ROOT, env=env, check=True)
            binary.chmod(0o755)
            for filename in ("README.md", "VALIDATION.md", "NOTICE.md", "LICENSE", "LICENSE.cct", "LICENSE.compress"):
                shutil.copy2(ROOT / filename, package / filename)
            shutil.copytree(ROOT / "deploy", package / "deploy")
            if os_name == "windows":
                (package / "start-client.cmd").write_bytes(b'@echo off\r\ncd /d "%~dp0"\r\nrelay.exe client --config client.json\r\npause\r\n')
            else:
                launcher = package / ("start-client.command" if os_name == "darwin" else "start-client.sh")
                launcher.write_text('#!/bin/sh\ncd -- "$(dirname -- "$0")" || exit 1\n./relay client --config client.json\n', encoding="utf-8")
                launcher.chmod(0o755)
            archive = dest / (name + ".zip")
            with zipfile.ZipFile(archive, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
                for path in sorted(package.rglob("*")):
                    if path.is_file():
                        z.write(path, path.relative_to(package.parent))
            sums.append(f"{hashlib.sha256(archive.read_bytes()).hexdigest()}  {archive.name}")
            print(f"Built {archive.name} ({archive.stat().st_size:,} bytes)", flush=True)
    source = dest / "session-relay-0.2.0-source.zip"
    with zipfile.ZipFile(source, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
        for path in sorted(ROOT.rglob("*")):
            rel = path.relative_to(ROOT)
            if not path.is_file() or any(p in {".git", "dist", "server-data", "__pycache__"} for p in rel.parts):
                continue
            if path == dest or dest in path.parents or path.name in {"relay", "relay.exe", "client.json"}:
                continue
            z.write(path, Path("session-relay-source") / rel)
    sums.append(f"{hashlib.sha256(source.read_bytes()).hexdigest()}  {source.name}")
    (dest / "SHA256SUMS.txt").write_text("\n".join(sums) + "\n", encoding="utf-8")

if __name__ == "__main__":
    main()
