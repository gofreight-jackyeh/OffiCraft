#!/usr/bin/env python3
"""Slack Socket Mode -> OffiCraft chat bridge.

Slack Socket Mode delivers events over an outbound WebSocket, so no public URL
is involved. This process holds that socket, acks each envelope, and posts the
message into OffiCraft as an ordinary chat message via POST /api/chat.

Two facts about the surrounding systems shape this file:

  * Slack does NOT buffer events while we are disconnected (docs.slack.dev,
    "using-socket-mode"). Anything sent while this process is down is LOST.
    There is no backfill to lean on, so we ack fast and keep the socket up.

  * OffiCraft's chat inlet has no dedup of its own, and Slack re-delivers an
    envelope that was not acked. Without the seen-event table below, one
    reconnect turns into duplicate chat messages.
"""

from __future__ import annotations

import json
import logging
import os
import signal
import sqlite3
import sys
import threading
import time
import tomllib
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any

from slack_sdk import WebClient
from slack_sdk.socket_mode import SocketModeClient
from slack_sdk.socket_mode.request import SocketModeRequest
from slack_sdk.socket_mode.response import SocketModeResponse

LOG = logging.getLogger("bridge")

# Slack re-sends an envelope we fail to ack, so the ack has to win the race
# against our own delivery work: ack first, deliver after.
DELIVER_RETRIES = 4
DELIVER_BACKOFF = 1.5

# Subtypes that are edits/removals/membership noise rather than something a
# colleague said. Kept deliberately small — an unknown subtype is delivered.
SKIP_SUBTYPES = {
    "message_changed",
    "message_deleted",
    "channel_join",
    "channel_leave",
    "bot_message",
}


def load_config(path: Path) -> dict[str, Any]:
    with path.open("rb") as fh:
        cfg = tomllib.load(fh)
    for key in ("slack.app_token", "officraft.base_url", "officraft.to"):
        section, name = key.split(".")
        if not cfg.get(section, {}).get(name):
            raise SystemExit(f"config {path}: missing required key [{section}].{name}")
    ignore = cfg.get("slack", {}).get("ignore_user_ids", [])
    if isinstance(ignore, str):
        cfg["slack"]["ignore_user_ids"] = [ignore]
    if not cfg["slack"]["app_token"].startswith("xapp-"):
        raise SystemExit("config: [slack].app_token must be an app-level token (xapp-...)")
    return cfg


class SeenEvents:
    """Slack event_id -> already delivered. Survives restarts on purpose:
    a reconnect right after a restart is exactly when Slack re-delivers."""

    def __init__(self, path: Path, keep_secs: int = 7 * 24 * 3600) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        self.db = sqlite3.connect(path, check_same_thread=False)
        self.db.execute(
            "CREATE TABLE IF NOT EXISTS seen (event_id TEXT PRIMARY KEY, ts REAL NOT NULL)"
        )
        self.db.commit()
        self.keep_secs = keep_secs
        self.lock = threading.Lock()

    def claim(self, event_id: str) -> bool:
        """True when this is the first sighting; False when it is a re-delivery."""
        with self.lock:
            try:
                self.db.execute(
                    "INSERT INTO seen (event_id, ts) VALUES (?, ?)", (event_id, time.time())
                )
            except sqlite3.IntegrityError:
                return False
            self.db.execute("DELETE FROM seen WHERE ts < ?", (time.time() - self.keep_secs,))
            self.db.commit()
            return True


class NameCache:
    """Slack ids are opaque; a message reading `U123 in C456` helps nobody.
    Resolution is best-effort — without a bot token we fall back to the id."""

    def __init__(self, web: WebClient | None) -> None:
        self.web = web
        self.users: dict[str, str] = {}
        self.channels: dict[str, str] = {}

    def user(self, uid: str) -> str:
        if not uid or not self.web:
            return uid or "?"
        if uid not in self.users:
            try:
                info = self.web.users_info(user=uid)
                p = info["user"]
                self.users[uid] = p.get("profile", {}).get("display_name") or p.get("real_name") or uid
            except Exception as exc:  # noqa: BLE001 - a name is never worth failing delivery
                LOG.warning("users_info(%s) failed: %s", uid, exc)
                self.users[uid] = uid
        return self.users[uid]

    def channel(self, cid: str) -> str:
        if not cid or not self.web:
            return cid or "?"
        if cid not in self.channels:
            try:
                self.channels[cid] = self.web.conversations_info(channel=cid)["channel"].get("name", cid)
            except Exception as exc:  # noqa: BLE001
                LOG.warning("conversations_info(%s) failed: %s", cid, exc)
                self.channels[cid] = cid
        return self.channels[cid]


class Credential:
    """The machine credential, on disk, kept alive by this process.

    OffiCraft mints a warden-class token with a finite lifetime (30 days by
    default) and offers `POST /api/machines/renew-credential`, which acts on the
    CALLER's own `sub` and takes no body — so a machine can only ever renew
    itself. We renew at two thirds of the lifetime, the cadence ocwarden uses, so
    a Mac that was asleep through one renewal window still has a valid token when
    it wakes.
    """

    def __init__(self, base_url: str, path: Path) -> None:
        self.base = base_url.rstrip("/")
        self.path = path
        self.lock = threading.Lock()
        self.token = ""
        self.expires_at = 0.0
        if path.exists():
            data = json.loads(path.read_text(encoding="utf-8"))
            self.token = data.get("token", "")
            self.expires_at = float(data.get("expires_at", 0))

    def _save(self, token: str, expires_in: int, machine_id: str) -> None:
        self.token = token
        self.expires_at = time.time() + int(expires_in)
        tmp = self.path.with_suffix(".tmp")
        tmp.write_text(
            json.dumps(
                {"token": token, "expires_at": self.expires_at, "machine_id": machine_id},
                indent=2,
            ),
            encoding="utf-8",
        )
        os.chmod(tmp, 0o600)
        tmp.replace(self.path)  # atomic: a torn credential file bricks the bridge
        LOG.info("credential stored for %s, valid %.1f days", machine_id, expires_in / 86400)

    def _call(self, path: str, body: dict[str, Any] | None, token: str | None) -> dict[str, Any]:
        headers = {"Content-Type": "application/json"}
        if token:
            headers["Authorization"] = f"Bearer {token}"
        req = urllib.request.Request(
            self.base + path,
            data=json.dumps(body or {}).encode(),
            method="POST",
            headers=headers,
        )
        with urllib.request.urlopen(req, timeout=15) as resp:
            return json.loads(resp.read())

    def claim(self, code: str) -> None:
        """One-time bootstrap: trade a console-issued claim code for a token."""
        r = self._call("/api/machines/claim", {"code": code}, None)
        self._save(r["token"], r["expires_in"], r["machine_id"])

    def renew(self) -> None:
        with self.lock:
            r = self._call("/api/machines/renew-credential", None, self.token)
            self._save(r["token"], r["expires_in"], r["machine_id"])

    def keep_fresh(self, stop: threading.Event) -> None:
        """Renew at 2/3 of remaining life; retry hourly on failure.

        Failure here is not fatal while the current token still works, so it
        logs and retries rather than taking the bridge down — but once the token
        does expire every delivery 401s, which is why the warning is loud.
        """
        while not stop.is_set():
            remaining = self.expires_at - time.time()
            wait = max(60.0, remaining / 3) if remaining > 0 else 3600.0
            if stop.wait(wait):
                return
            try:
                self.renew()
            except Exception as exc:  # noqa: BLE001
                LOG.warning("credential renewal failed (%.1f days left): %s",
                            max(0.0, self.expires_at - time.time()) / 86400, exc)


class OffiCraft:
    def __init__(self, base_url: str, cred: Credential, to: str) -> None:
        self.url = base_url.rstrip("/") + "/api/chat"
        self.cred = cred
        self.to = to

    def post(self, body: str, meta: dict[str, Any]) -> None:
        payload = json.dumps({"to": self.to, "body": body, "meta": meta}).encode()
        last: Exception | None = None
        renewed = False
        for attempt in range(DELIVER_RETRIES):
            # Rebuilt every attempt: a renewal between attempts must not be
            # undone by a stale Authorization header captured once up front.
            req = urllib.request.Request(
                self.url,
                data=payload,
                method="POST",
                headers={
                    "Content-Type": "application/json",
                    "Authorization": f"Bearer {self.cred.token}",
                },
            )
            try:
                with urllib.request.urlopen(req, timeout=15) as resp:
                    LOG.info("delivered -> %s (HTTP %s)", self.to, resp.status)
                    return
            except urllib.error.HTTPError as exc:
                detail = exc.read()[:400].decode("utf-8", "replace")
                # An expired token is the one 4xx worth one more go: renew, retry
                # once. Otherwise every message after expiry would dead-letter.
                if exc.code == 401 and not renewed:
                    renewed = True
                    LOG.warning("401 from OffiCraft — renewing credential and retrying")
                    try:
                        self.cred.renew()
                        continue
                    except Exception as renew_exc:  # noqa: BLE001
                        raise RuntimeError(f"401 and renewal failed: {renew_exc}") from exc
                if 400 <= exc.code < 500:
                    raise RuntimeError(f"HTTP {exc.code} from OffiCraft: {detail}") from exc
                last = exc
            except Exception as exc:  # noqa: BLE001
                last = exc
            time.sleep(DELIVER_BACKOFF ** attempt)
        raise RuntimeError(f"delivery failed after {DELIVER_RETRIES} attempts: {last}")


def render(event: dict[str, Any], names: NameCache) -> str:
    """One human-readable line, then the text.

    A DM has no channel name — rendering its `D0…` id told the reader nothing,
    so the conversation KIND (`channel_type`) decides the prefix instead.
    """
    who = names.user(event.get("user", ""))
    kind = event.get("channel_type", "")
    if kind == "im":
        head = f"[Slack 私訊] {who}:"
    elif kind == "mpim":
        head = f"[Slack 群組私訊] {who}:"
    else:
        head = f"[Slack #{names.channel(event.get('channel', ''))}] {who}:"
    text = (event.get("text") or "").strip()
    return f"{head}\n{text}" if text else f"{head} (無文字內容)"


def make_handler(oc: OffiCraft, seen: SeenEvents, names: NameCache, deadletter: Path,
                 ignore_users: frozenset[str] = frozenset()):
    def handle(client: SocketModeClient, req: SocketModeRequest) -> None:
        # Ack unconditionally and first. A slow ack is re-delivered, and a
        # re-delivery we then also process is a duplicate chat message.
        client.send_socket_mode_response(SocketModeResponse(envelope_id=req.envelope_id))

        if req.type != "events_api":
            return
        payload = req.payload or {}
        event = payload.get("event") or {}
        if event.get("type") != "message":
            return
        if event.get("bot_id") or event.get("subtype") in SKIP_SUBTYPES:
            return
        # The owner reads what OTHERS said; his own outgoing lines are already
        # his. Dropped before the dedup table so they do not fill it either.
        if event.get("user") in ignore_users:
            LOG.info("skip own message from %s", event.get("user"))
            return

        # Dedup on the MESSAGE, not on the envelope. Once a user-token
        # authorization exists alongside the bot one, the same message is
        # visible to both and Slack may mint a separate event_id per
        # authorization — keying on event_id would let that through twice.
        # (channel, ts) names the message itself and is stable across both
        # re-delivery and authorization.
        chan, ts = event.get("channel"), event.get("ts")
        key = f"{chan}:{ts}" if chan and ts else payload.get("event_id", "")
        if not key:
            LOG.warning("event with no usable dedup key, dropping: %s", payload.get("event_id"))
            return
        if not seen.claim(key):
            LOG.info("skip duplicate %s", key)
            return
        auth = (payload.get("authorizations") or [{}])[0]
        LOG.info("accepted %s (authorization: %s)", key,
                 "bot" if auth.get("is_bot") else f"user {auth.get('user_id') or '?'}")

        meta = {
            "source": "slack-socket-mode",
            "slack": {
                "event_id": payload.get("event_id"),
                "channel": event.get("channel"),
                "user": event.get("user"),
                "ts": event.get("ts"),
                "thread_ts": event.get("thread_ts"),
            },
        }
        try:
            oc.post(render(event, names), meta)
        except Exception as exc:  # noqa: BLE001
            # Already acked, so Slack will not send it again — the only way this
            # message survives is on our own disk.
            LOG.error("DELIVERY FAILED %s: %s", event_id, exc)
            with deadletter.open("a", encoding="utf-8") as fh:
                fh.write(json.dumps({"at": time.time(), "error": str(exc), "payload": payload},
                                    ensure_ascii=False) + "\n")

    return handle


def main() -> int:
    logging.basicConfig(
        level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s"
    )
    cfg_path = Path(os.environ.get("BRIDGE_CONFIG", Path.home() / ".officraft-slack-bridge/config.toml"))
    cfg = load_config(cfg_path)
    state_dir = Path(cfg.get("bridge", {}).get("state_dir", cfg_path.parent)).expanduser()
    state_dir.mkdir(parents=True, exist_ok=True)

    cred = Credential(cfg["officraft"]["base_url"], state_dir / "credential.json")

    # One-time bootstrap: `bridge.py claim <code>` trades the console's claim
    # code for a machine token, then exits. Everything after needs that token.
    if len(sys.argv) > 2 and sys.argv[1] == "claim":
        cred.claim(sys.argv[2])
        print(f"claimed; credential written to {cred.path}")
        return 0
    if not cred.token:
        raise SystemExit(
            f"no credential at {cred.path} — run:  uv run python bridge.py claim <claim-code>"
        )

    bot_token = cfg["slack"].get("bot_token")
    web = WebClient(token=bot_token) if bot_token else None
    if not web:
        LOG.warning("no [slack].bot_token — user/channel ids will not be resolved to names")

    oc = OffiCraft(cfg["officraft"]["base_url"], cred, cfg["officraft"]["to"])
    client = SocketModeClient(app_token=cfg["slack"]["app_token"], web_client=web or WebClient())
    client.socket_mode_request_listeners.append(
        make_handler(
            oc,
            SeenEvents(state_dir / "seen.db"),
            NameCache(web),
            state_dir / "deadletter.jsonl",
            frozenset(cfg["slack"].get("ignore_user_ids", [])),
        )
    )

    stop = threading.Event()
    for sig in (signal.SIGTERM, signal.SIGINT):
        signal.signal(sig, lambda *_: stop.set())

    threading.Thread(target=cred.keep_fresh, args=(stop,), daemon=True, name="renew").start()

    LOG.info("connecting to Slack (socket mode); delivering to %s", oc.to)
    client.connect()
    stop.wait()
    LOG.info("shutting down")
    client.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
