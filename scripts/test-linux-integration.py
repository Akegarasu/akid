#!/usr/bin/env python3
"""Exercise a built akid binary in disposable XDG directories (Linux only).

Usage: python3 scripts/test-linux-integration.py /absolute/path/to/akid
The existing user's daemon, state and systemd configuration are never used.
"""

import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tempfile
import time


def main():
    binary = str(Path(sys.argv[1]).resolve(strict=True))
    if sys.platform != "linux":
        raise SystemExit("this integration check requires Linux")
    with tempfile.TemporaryDirectory(prefix="akid-integration-") as temporary:
        root = Path(temporary)
        runtime = root / "runtime"
        runtime.mkdir(mode=0o700)
        env = os.environ | {
            "XDG_STATE_HOME": str(root / "state"),
            "XDG_RUNTIME_DIR": str(runtime),
            "XDG_CONFIG_HOME": str(root / "config"),
        }
        endpoint = runtime / "akid.sock"
        daemon = None
        tracked = set()

        def identity(pid):
            try:
                data = Path(f"/proc/{pid}/stat").read_text()
                fields = data[data.rindex(")") + 2:].split()
                return (int(fields[19]), fields[0], int(fields[2]))
            except FileNotFoundError:
                return None

        def remember(info):
            running = info["runtime"]
            if running.get("pid"):
                tracked.add((running["pid"], running["start_time"]))
            return info

        def rpc(method, params=None):
            with socket.socket(socket.AF_UNIX) as connection:
                connection.settimeout(5)
                connection.connect(str(endpoint))
                request = {"protocol": 2, "id": 1, "method": method}
                if params is not None:
                    request["params"] = params
                connection.sendall(json.dumps(request).encode() + b"\n")
                with connection.makefile("rb") as stream:
                    response = json.loads(stream.readline())
                assert response["protocol"] == 2 and response["id"] == 1, response
                if "error" in response:
                    raise AssertionError(response["error"])
                return response.get("result")

        def cli(*args, success=True):
            completed = subprocess.run([binary, *args], env=env, cwd=root,
                                       capture_output=True, text=True, timeout=20)
            if success:
                assert completed.returncode == 0, (args, completed.stdout, completed.stderr)
            else:
                assert completed.returncode != 0, (args, "unexpected success")
            return completed

        def wait_for(check, description):
            deadline = time.monotonic() + 8
            while time.monotonic() < deadline:
                try:
                    result = check()
                    if result:
                        return result
                except (FileNotFoundError, ConnectionRefusedError):
                    pass
                time.sleep(0.025)
            raise AssertionError("timed out: " + description)

        def start_daemon():
            nonlocal daemon
            daemon = subprocess.Popen([binary, "daemon", "run"], env=env, cwd=root,
                                      stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
            wait_for(lambda: rpc("daemon.ping"), "daemon socket ready")

        def config(mode="one", command="/bin/sh"):
            # JSON quoting is also valid for these TOML basic strings.
            script = "printf '%s\\n' \"$MODE\"; while :; do sleep 1; done"
            return ("[[process]]\nname='api'\ncommand=" + json.dumps(command)
                    + "\nargs=" + json.dumps(["-c", script])
                    + "\nrestart='never'\nstop_timeout='200ms'\n[process.env]\nMODE="
                    + json.dumps(mode) + "\n")

        path = root / "akid.toml"
        path.write_text(config())
        try:
            assert "valid configuration" in cli("apply", "--check", str(path)).stdout
            assert not endpoint.exists(), "--check started a daemon"
            start_daemon()
            capabilities = rpc("daemon.capabilities")
            assert "config.apply" in capabilities["methods"], capabilities

            # The CLI accepts one quoted command string and resolves a tool
            # from its own PATH before handing it to a daemon started with a
            # different environment (the ~/.local/bin/uv case).
            tool_dir = root / "tools"
            tool_dir.mkdir()
            uv = tool_dir / "uv"
            uv.write_text("#!/bin/sh\nprintf '%s\\n' \"$*\"\nwhile :; do sleep 1; done\n")
            uv.chmod(0o700)
            env["PATH"] = str(tool_dir) + os.pathsep + env["PATH"]
            cli("start", "uv run bot.py", "--name=chino-bot", "--restart=never")
            quoted = remember(rpc("process.get", {"id": "chino-bot"}))
            assert quoted["config"]["command"] == str(uv), quoted
            assert quoted["config"]["args"] == ["run", "bot.py"], quoted
            cli("stop", "1")
            cli("delete", "1", "--purge")

            assert "created" in cli("apply", str(path)).stdout
            first = remember(rpc("process.get", {"id": "api"}))
            first_pid = first["runtime"]["pid"]
            assert first["runtime"]["status"] == "running", first
            assert first["config"]["cwd"] == str(root), first
            wait_for(lambda: "one" in cli("logs", "api").stdout, "first log output")
            assert "unchanged" in cli("apply", str(path)).stdout
            assert rpc("process.get", {"id": "api"})["runtime"]["pid"] == first_pid

            # A later invalid entry must leave the earlier running entry intact.
            path.write_text(config("must-not-apply") + "\n[[process]]\nname='broken'\n")
            cli("apply", str(path), success=False)
            assert rpc("process.get", {"id": "api"})["config"]["env"]["MODE"] == "one"

            path.write_text(config("two"))
            assert "updated (running)" in cli("apply", str(path)).stdout
            second = remember(rpc("process.get", {"id": "api"}))
            assert second["config"]["id"] == first["config"]["id"]
            assert second["runtime"]["pid"] != first_pid
            wait_for(lambda: "two" in cli("logs", "api").stdout, "updated log output")

            # No implicit pruning when a file omits a managed process.
            empty = root / "empty.toml"
            empty.write_text("process = []\n")
            cli("apply", str(empty))
            assert len(rpc("process.list")) == 1

            # True daemon death: its process survives, then is adopted by the
            # replacement daemon without changing the process ID.
            old_pid = second["runtime"]["pid"]
            daemon.kill()
            daemon.wait(timeout=5)
            endpoint.unlink(missing_ok=True)
            start_daemon()
            adopted = remember(rpc("process.get", {"id": "api"}))
            assert adopted["runtime"]["pid"] == old_pid, adopted
            cli("stop", "api")
            assert rpc("process.get", {"id": "api"})["runtime"]["status"] == "stopped"
            assert "unchanged" in cli("apply", str(path)).stdout
            assert rpc("process.get", {"id": "api"})["runtime"]["status"] == "stopped"

            # A bad executable is a runtime error, not a fake successful apply.
            path.write_text(config("bad", str(root / "missing-executable")))
            failed = cli("apply", str(path), success=False)
            assert "SPAWN_FAILED" in failed.stderr, failed.stderr
            path.write_text(config("recovered"))
            cli("apply", str(path))
            remember(rpc("process.get", {"id": "api"}))
            cli("delete", "api", "--purge")
            assert rpc("process.list") == []
            assert not list((root / "state" / "akid" / "logs").glob("api.*"))

            # Exercise CLI startup wiring using stubs; never invoke host systemd.
            stubdir = root / "stubs"
            stubdir.mkdir()
            for command in ("systemctl", "loginctl"):
                stub = stubdir / command
                stub.write_text("#!/bin/sh\n" + ("printf 'no\\n'\n" if command == "loginctl" else "exit 0\n"))
                stub.chmod(0o700)
            env["PATH"] = str(stubdir) + os.pathsep + env["PATH"]
            installed = cli("startup", "install")
            assert "linger is disabled" in installed.stderr, installed.stderr
            unit = root / "config" / "systemd" / "user" / "akid.service"
            assert "Restart=on-failure" in unit.read_text()
            cli("startup", "uninstall")
            assert not unit.exists()
            assert rpc("daemon.ping"), "startup helper stopped the daemon"

            rpc("daemon.shutdown")
            assert daemon.wait(timeout=10) == 0
            for pid, token in tracked:
                stat = identity(pid)
                assert stat is None or stat[0] != token or stat[1] in ("Z", "X"), ("live test process", pid, stat)
            groups = {pid for pid, _ in tracked}
            for process in Path("/proc").iterdir():
                if process.name.isdigit():
                    stat = identity(int(process.name))
                    assert stat is None or stat[2] not in groups or stat[1] in ("Z", "X"), ("live test group member", process.name, stat)
            print("PASS: validation, capabilities, apply create/no-op/update, logs, invalid batch, no prune, crash/adopt/stop, runtime failure/recovery, purge, startup stubs, shutdown; no live test group members")
        finally:
            if daemon is not None and daemon.poll() is None:
                try:
                    rpc("daemon.shutdown")
                    daemon.wait(timeout=10)
                except Exception:
                    daemon.kill()
                    daemon.wait(timeout=5)
            # Identity-check cleanup targets: never match arbitrary process names.
            for pid, token in tracked:
                stat = identity(pid)
                if stat is not None and stat[0] == token and stat[2] == pid:
                    try:
                        os.killpg(pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass


if __name__ == "__main__":
    main()
