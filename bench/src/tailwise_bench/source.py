"""The exact source a run used, for a tree with uncommitted changes.

A run records the commit it started from and a patch of everything not
committed: changes to tracked files and new files git does not ignore.
Applying the patch to that commit (`git apply source.patch`) rebuilds the
tree, and the patch's SHA-256 identifies it.
"""

from __future__ import annotations

import hashlib
import subprocess
from pathlib import Path
from typing import Any


def _git(repo: Path, *args: str, check: bool = True) -> str:
    out = subprocess.run(["git", *args], cwd=repo, capture_output=True, text=True)
    if check and out.returncode != 0:
        raise RuntimeError(f"git {' '.join(args)}: {out.stderr.strip()}")
    return out.stdout


def patch(repo: Path) -> str:
    """Uncommitted changes to tracked files, then every untracked, unignored
    file as a new-file diff. Nothing is staged or written to the repository."""
    parts = [_git(repo, "diff", "--binary", "HEAD")]
    for name in sorted(_git(repo, "ls-files", "--others", "--exclude-standard", "-z").split("\0")):
        if name:
            # --no-index exits 1 when the files differ, which they always do here.
            parts.append(_git(repo, "diff", "--binary", "--no-index", "--", "/dev/null", name, check=False))
    return "".join(parts)


def record(repo: Path, out_dir: Path) -> dict[str, Any]:
    """Writes out_dir/source.patch and returns what identifies the source."""
    text = patch(repo)
    path = out_dir / "source.patch"
    path.write_text(text)
    return {
        "base_sha": _git(repo, "rev-parse", "HEAD").strip(),
        "dirty": bool(text),
        "patch_file": path.name,
        "patch_sha256": hashlib.sha256(text.encode()).hexdigest(),
        "patch_bytes": len(text.encode()),
        "rebuild": "git checkout <base_sha> && git apply source.patch",
    }
