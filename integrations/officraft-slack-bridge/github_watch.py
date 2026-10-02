#!/usr/bin/env python3
"""GitHub review-request watcher -> OffiCraft chat.

Runs ONE pass and exits; launchd re-runs it hourly. A long-lived process would
buy nothing here — there is no connection to hold, and a crash between runs
would be invisible.

Why polling rather than a webhook: GitHub cannot subscribe to a single ACTION,
so a `pull_request` webhook delivers every opened/synchronize/labeled/closed
event too, at ~26 KB of raw JSON each, into a chat body that folds at 4,000
chars. The search API answers the actual question — "which open PRs request my
review" — in one call.

FIRST RUN SEEDS, IT DOES NOT REPORT. The standing backlog was >100 PRs; sending
that as a welcome message would bury the signal this exists to deliver.
"""

from __future__ import annotations

import json
import logging
import os
import subprocess
import sys
import tomllib
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).parent))
from bridge import Credential, OffiCraft, SeenEvents  # noqa: E402

LOG = logging.getLogger("gh-watch")

# One chat body caps at 4,000 chars; a PR line is ~120. Stop well short and say
# how many were held back, rather than silently losing the tail to a fold.
MAX_LINES = 20


def gh_search(query: str) -> list[dict[str, Any]]:
    """One search/issues page via the gh CLI.

    gh is invoked rather than the REST API directly so this process never
    handles the GitHub token: gh reads its own credential from
    ~/.config/gh/hosts.yml, which a launchd job can reach (it is a file, not a
    keychain entry).
    """
    out = subprocess.run(
        ["gh", "api", "-X", "GET", "search/issues",
         "--field", f"q={query}", "--field", "per_page=100"],
        capture_output=True, text=True, timeout=60,
    )
    if out.returncode != 0:
        raise RuntimeError(f"gh failed ({out.returncode}): {out.stderr.strip()[:300]}")
    return json.loads(out.stdout).get("items", [])


def queries(cfg: dict[str, Any]) -> list[tuple[str, str]]:
    g = cfg["github"]
    excl = " ".join(f"-author:{a}" for a in g.get("exclude_authors", []))
    base = "is:open is:pr archived:false"
    out = [("個人指名", f"{base} review-requested:@me {excl}".strip())]
    for team in g.get("teams", []):
        out.append((f"team {team.split('/')[-1]}",
                    f"{base} team-review-requested:{team} {excl}".strip()))
    return out


def render(new: list[tuple[str, dict[str, Any]]]) -> str:
    head = f"[GitHub] {len(new)} 筆新的 review request"
    lines = []
    for why, it in new[:MAX_LINES]:
        url = it.get("html_url", "")
        repo = "/".join(url.split("/")[3:5]) if url else "?"
        lines.append(
            f"• {repo} #{it.get('number')} — {(it.get('title') or '').strip()[:90]}\n"
            f"  {why} · 作者 {it.get('user', {}).get('login', '?')} · {url}"
        )
    if len(new) > MAX_LINES:
        lines.append(f"…另有 {len(new) - MAX_LINES} 筆未列出")
    return head + "\n\n" + "\n".join(lines)


def main() -> int:
    logging.basicConfig(level=logging.INFO,
                        format="%(asctime)s %(levelname)s %(name)s %(message)s")
    cfg_path = Path(os.environ.get(
        "BRIDGE_CONFIG", Path.home() / ".officraft-slack-bridge/config.toml"))
    cfg = tomllib.load(cfg_path.open("rb"))
    if "github" not in cfg:
        raise SystemExit(f"{cfg_path}: missing [github] section")

    state_dir = Path(cfg.get("bridge", {}).get("state_dir", cfg_path.parent)).expanduser()
    seen = SeenEvents(state_dir / "github_seen.db", keep_secs=180 * 24 * 3600)
    seeded = state_dir / "github_seeded"

    found: dict[str, tuple[str, dict[str, Any]]] = {}
    for why, q in queries(cfg):
        items = gh_search(q)
        LOG.info("%s: %d 筆 (%s)", why, len(items), q)
        for it in items:
            # A PR requested BOTH personally and via a team is still one PR; the
            # url is its identity and the first reason found wins.
            found.setdefault(it["html_url"], (why, it))

    fresh = [(why, it) for url, (why, it) in found.items() if seen.claim(url)]

    if not seeded.exists():
        seeded.write_text(f"seeded {len(found)} open review requests\n", encoding="utf-8")
        LOG.info("first run: seeded %d existing requests, reporting none", len(found))
        return 0

    if not fresh:
        LOG.info("no new review requests (%d open in total)", len(found))
        return 0

    cred = Credential(cfg["officraft"]["base_url"], state_dir / "credential.json")
    if not cred.token:
        raise SystemExit("no OffiCraft credential — run bridge.py claim <code> first")
    oc = OffiCraft(cfg["officraft"]["base_url"], cred,
                   cfg["github"].get("to") or cfg["officraft"]["to"])
    oc.post(render(fresh), {"source": "github-review-watch",
                            "github": {"count": len(fresh),
                                       "urls": [i.get("html_url") for _, i in fresh]}})
    LOG.info("reported %d new review request(s)", len(fresh))
    return 0


if __name__ == "__main__":
    sys.exit(main())
