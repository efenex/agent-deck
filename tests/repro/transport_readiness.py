#!/usr/bin/env python3
"""Container-only CLI reproduction. Exit 0 healthy, 1 defect, 2 harness error.

Usage: python3 transport_readiness.py /absolute/path/to/agent-deck
The peer is a local dummy Unix listener in a private tmux server, never an agent.
"""

import json
import os
from pathlib import Path
import selectors
import shlex
import socket
import sqlite3
import subprocess
import sys
import tempfile
import time
import tty


def peer(root):
    root = Path(root)
    tty.setraw(0)
    listener = socket.socket(socket.AF_UNIX)
    listener.bind(str(root / "peer.sock"))
    listener.listen()
    pid = os.getpid()
    record = {"pid": pid, "sessionId": "readiness-fixture", "peerProtocol": 1,
              "messagingSocketPath": str(root / "peer.sock")}
    records = root / "home/.claude/sessions"
    records.mkdir(parents=True)
    (records / f"{pid}.json").write_text(json.dumps(record))
    print("\r\n✳ Working… (28s · esc to interrupt)\r\n", flush=True)
    (root / "peer-ready").write_text(str(pid))
    selector = selectors.DefaultSelector()
    selector.register(listener, selectors.EVENT_READ)
    selector.register(0, selectors.EVENT_READ)
    while True:
        for key, _ in selector.select():
            if key.fileobj == 0:
                data = os.read(0, 4096)
                if not data:
                    return
                with (root / "keyboard").open("ab") as output:
                    output.write(data)
            else:
                connection, _ = listener.accept()
                with connection, (root / "received").open("ab") as output:
                    while data := connection.recv(65536):
                        output.write(data)


def run(binary):
    if not Path("/.dockerenv").exists() and not Path("/run/.containerenv").exists():
        raise RuntimeError("this reproduction requires a disposable container")
    binary = str(Path(binary).resolve())
    with tempfile.TemporaryDirectory(prefix="ad-ready-") as directory:
        root = Path(directory)
        env = {"PATH": "/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin", "TERM": "xterm-256color",
               "HOME": str(root / "home"), "TMUX_TMPDIR": str(root / "tmux"),
               "XDG_CONFIG_HOME": str(root / "config"), "XDG_DATA_HOME": str(root / "data"),
               "XDG_CACHE_HOME": str(root / "cache"), "XDG_STATE_HOME": str(root / "state"),
               "AGENTDECK_SKIP_UPDATE_CHECK": "1"}
        for key in ("HOME", "TMUX_TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME"):
            Path(env[key]).mkdir()
        config = root / "config/agent-deck"
        config.mkdir()
        (config / "config.toml").write_text('send_transport = "auto"\n')

        def command(args, check=True):
            result = subprocess.run(args, env=env, cwd=root, text=True, capture_output=True, timeout=15)
            if check and result.returncode:
                raise RuntimeError(f"{args}: rc={result.returncode}: {result.stdout} {result.stderr}")
            return result

        command([binary, "add", "--json", "--no-parent", "-c", "claude", "-t", "readiness-fixture", str(root)])
        databases = list(root.rglob("state.db"))
        if len(databases) != 1:
            raise RuntimeError(f"expected one fixture database, got {databases}")
        connection = sqlite3.connect(databases[0])
        instance_id = connection.execute("select id from instances").fetchone()[0]
        socket_name = "readiness-repro"
        tmux = ["tmux", "-L", socket_name]
        try:
            peer_command = shlex.join([sys.executable, str(Path(__file__).resolve()), "--peer", str(root)])
            command(tmux + ["new-session", "-d", "-s", "readiness-fixture", "-x", "120", "-y", "30", "exec " + peer_command])
            deadline = time.monotonic() + 5
            while not (root / "peer-ready").exists():
                if time.monotonic() >= deadline:
                    raise RuntimeError("dummy peer did not start")
                time.sleep(0.02)
            connection.execute("update instances set tmux_session=?, tmux_socket_name=?, status='waiting', tool_data=? where id=?",
                               ("readiness-fixture", socket_name, json.dumps({"claude_session_id": "readiness-fixture"}), instance_id))
            connection.commit()
            hooks = root / "data/agent-deck/hooks"
            hooks.mkdir(parents=True, exist_ok=True)

            def attempt(name, status):
                (hooks / (instance_id + ".json")).write_text(json.dumps({
                    "status": status, "session_id": "readiness-fixture", "ts": int(time.time()),
                    "event": "Stop" if status == "waiting" else "UserPromptSubmit"}))
                started = time.monotonic()
                result = command([binary, "session", "send", "--json", "--no-tag", "--defer-if-busy",
                                  "--defer-timeout", "700ms", "--timeout", "1s", instance_id, name], check=False)
                try:
                    payload = json.loads(result.stdout)
                except json.JSONDecodeError as error:
                    raise RuntimeError(f"invalid CLI output: {result.stdout} {result.stderr}") from error
                observation = {"utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                               "case": name, "rc": result.returncode, "elapsed_ms": round((time.monotonic() - started) * 1000),
                               "payload": payload, "stderr": result.stderr}
                print(json.dumps(observation), flush=True)
                return observation

            waiting = attempt("waiting-hook-active-pane", "waiting")
            running = attempt("running-hook-must-defer", "running")
            (root / "peer.sock").unlink()
            fallback = attempt("unavailable-socket-no-composer", "waiting")
            received = (root / "received").read_text() if (root / "received").exists() else ""
            keyboard_bytes = (root / "keyboard").stat().st_size if (root / "keyboard").exists() else 0
            healthy = (waiting["rc"] == 0 and "queued_socket" in json.dumps(waiting["payload"])
                       and "waiting-hook-active-pane" in received
                       and running["rc"] != 0 and "running-hook-must-defer" not in received
                       and fallback["rc"] != 0 and keyboard_bytes == 0)
            specific_defect = waiting["rc"] != 0 and "not ready" in json.dumps(waiting["payload"]) and not received and keyboard_bytes == 0
            print(json.dumps({"oracle": "socket bypass, fresh running deferral, guarded fallback", "healthy": healthy,
                              "specific_defect": specific_defect, "keyboard_bytes": keyboard_bytes}), flush=True)
            if healthy:
                return 0
            if specific_defect:
                return 1
            raise RuntimeError("output matches neither the defect nor the healthy oracle")
        finally:
            connection.close()
            command(tmux + ["kill-server"], check=False)


if __name__ == "__main__":
    try:
        if len(sys.argv) == 3 and sys.argv[1] == "--peer":
            peer(sys.argv[2])
        else:
            sys.exit(run(sys.argv[1]))
    except Exception as error:
        print(f"HARNESS ERROR: {error}", file=sys.stderr)
        sys.exit(2)
