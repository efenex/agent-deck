"""Lifecycle checks for the real fixture, without a tmux dependency."""
import json
import os
from pathlib import Path
import pty
import signal
import subprocess
import sys
import tempfile
import termios
import time
import unittest


class ClaudeLikeFakeLifecycle(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="fake-claude-lifecycle-")
        self.addCleanup(self.directory.cleanup)
        root = Path(self.directory.name).resolve()
        script = root / "fake-claude.py"
        script.write_bytes(Path(__file__).with_name("claude_like_fake.py").read_bytes())
        transcript = str(root / "session.jsonl")
        (root / "fake-running.json").write_text(json.dumps({str(root): transcript}))
        Path(transcript + ".input-control.json").write_text('{"id":"idle","phase":"idle"}')
        self.master, self.slave = pty.openpty()
        self.addCleanup(os.close, self.slave)
        self.addCleanup(self.close_master)
        # A restart wrapper can pass ignored HUP to its children. The fixture
        # must restore termination instead of inheriting that disposition.
        previous = signal.signal(signal.SIGHUP, signal.SIG_IGN)
        try:
            self.process = subprocess.Popen(
                [sys.executable, "-I", str(script)], cwd=root,
                stdin=self.slave, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
                start_new_session=True,
            )
        finally:
            signal.signal(signal.SIGHUP, previous)
        self.addCleanup(self.reap)
        deadline = time.monotonic() + 3
        while not Path(transcript + ".input-state.json").exists():
            if self.process.poll() is not None:
                self.fail("fixture exited before readiness: " + self.process.stderr.read().decode())
            if time.monotonic() >= deadline:
                self.fail("fixture did not become ready")
            time.sleep(0.01)

    def close_master(self):
        if self.master is not None:
            os.close(self.master)
            self.master = None

    def reap(self):
        if self.process.poll() is None:
            self.process.kill()
        self.process.wait(timeout=3)
        self.process.stderr.close()

    def assert_exits(self):
        try:
            self.process.wait(timeout=1)
        except subprocess.TimeoutExpired:
            self.fail("fake-claude outlived stdin/HUP shutdown")

    def test_eof(self):
        # Canonical VEOF yields a real zero-byte read while the PTY and output
        # remain usable. It exposes the hot EOF loop without relying on EIO.
        attrs = termios.tcgetattr(self.slave)
        attrs[3] |= termios.ICANON
        attrs[6][termios.VEOF] = b"\x04"
        termios.tcsetattr(self.slave, termios.TCSANOW, attrs)
        os.write(self.master, b"\x04")
        self.assert_exits()
        self.assertEqual(self.process.returncode, 0)

    def test_hup_even_when_inherited_ignored(self):
        os.kill(self.process.pid, signal.SIGHUP)
        self.assert_exits()

    def test_closed_terminal(self):
        self.close_master()
        self.assert_exits()
        self.assertEqual(self.process.returncode, 0)

    def test_idle_blocks_and_escape_only_input_is_not_eof(self):
        os.write(self.master, b"\x1b[200~\x1b[201~")
        time.sleep(0.2)
        self.assertIsNone(self.process.poll(), "escape-only input must not terminate the fixture")
        os.kill(self.process.pid, signal.SIGTERM)
        _, status, usage = os.wait4(self.process.pid, 0)
        self.process.returncode = os.waitstatus_to_exitcode(status)
        self.assertLess(usage.ru_utime + usage.ru_stime, 0.15, "idle fixture burned CPU")


if __name__ == "__main__":
    unittest.main()
