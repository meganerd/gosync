#!/usr/bin/env python3
"""Local-only tests: no SSH, remote commands, or existing benchmark artifacts."""
import argparse
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shlex
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location(
    "benchmark_transfer", Path(__file__).with_name("benchmark-transfer.py"))
benchmark = importlib.util.module_from_spec(spec)
spec.loader.exec_module(benchmark)


class ArgumentTests(unittest.TestCase):
    def parse(self, *extra):
        argv = ["benchmark-transfer.py", "--host", "unused", "--source", "source",
                "--destination", "/unused", "--gosync", "gosync", "--output", "reports"]
        with mock.patch.object(sys, "argv", argv + list(extra)):
            return benchmark.parse_args()

    def test_buffer_size_omitted(self):
        self.assertIsNone(self.parse().buffer_size)

    def test_buffer_size_valid(self):
        for value in (4096, 262144, 4194304):
            with self.subTest(value=value):
                self.assertEqual(self.parse("--buffer-size", str(value)).buffer_size, value)

    def test_buffer_size_invalid(self):
        for value in ("4095", "4194305", "0", "-1", "262144.0", "invalid"):
            with self.subTest(value=value), contextlib.redirect_stderr(io.StringIO()), \
                    self.assertRaises(SystemExit) as error:
                self.parse("--buffer-size", value)
            self.assertEqual(error.exception.code, 2)


class CommandTests(unittest.TestCase):
    def test_success(self):
        result = benchmark.run_command(
            [sys.executable, "-c", "print('ok')"], timeout=5)
        self.assertEqual(benchmark.checked(result), "ok\n")
        self.assertFalse(result["timed_out"])
        self.assertGreater(result["wall_seconds"], 0)

    def test_deadline_kills_and_reaps_child(self):
        children = []
        popen = subprocess.Popen

        def capture(*args, **kwargs):
            child = popen(*args, **kwargs)
            children.append(child)
            return child

        start = time.monotonic()
        with mock.patch.object(subprocess, "Popen", side_effect=capture):
            result = benchmark.run_command(
                [sys.executable, "-c", "import time; time.sleep(60)"], timeout=.1)
        self.assertTrue(result["timed_out"])
        self.assertEqual(result["returncode"], -signal.SIGKILL)
        self.assertLess(time.monotonic() - start, 10)
        self.assertIsNotNone(children[0].poll())
        with self.assertRaises(ChildProcessError):
            os.waitpid(children[0].pid, os.WNOHANG)
        with self.assertRaises(RuntimeError):
            benchmark.checked(result)


class HelperTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.config = {
            "token": "test-token", "destination": str(self.root),
            "binary_dir": str(self.root / "gosync-bench-test-token-bin"),
            "data_dir": str(self.root / "gosync-bench-test-token-data"),
            "size": 1, "reserve": 0, "binary_size": 1,
            "helper_timeout": 5, "lifetime": 1,
            "supervisor": benchmark.SUPERVISOR,
            "rsync_wrapper": benchmark.RSYNC_WRAPPER,
        }

    def helper(self, action, config=None):
        config = dict(self.config if config is None else config, action=action)
        output = io.StringIO()
        with mock.patch.object(sys, "argv", ["helper", json.dumps(config)]), \
                mock.patch.object(signal, "signal"), \
                mock.patch.object(signal, "setitimer"), \
                mock.patch.object(Path, "home", return_value=self.root), \
                mock.patch.object(benchmark.shutil, "which", return_value="rsync"), \
                mock.patch.object(subprocess, "check_output", return_value="rsync test"), \
                contextlib.redirect_stdout(output):
            try:
                exec(compile(benchmark.REMOTE, "<remote-helper>", "exec"), {})
            except SystemExit as error:
                if error.code != 0:
                    raise
        return json.loads(output.getvalue())

    def test_discovery_is_read_only(self):
        self.assertEqual(self.helper("discover"), {
            "home": str(self.root), "destination": str(self.root)})
        self.assertEqual(list(self.root.iterdir()), [])

    def test_prepare_and_cleanup(self):
        self.helper("prepare")
        for key in ("binary_dir", "data_dir"):
            path = Path(self.config[key])
            self.assertEqual((path / ".owner").read_text(), "test-token")
            self.assertEqual(path.stat().st_mode & 0o777, 0o700)
        self.helper("cleanup")
        self.helper("cleanup")  # Already-removed owned paths are harmless.
        self.assertEqual(list(self.root.iterdir()), [])

    def test_prepare_refuses_existing_paths_even_with_matching_marker(self):
        for key in ("binary_dir", "data_dir"):
            with self.subTest(key=key):
                path = Path(self.config[key])
                path.mkdir()
                marker = path / ".owner"
                marker.write_text("test-token")
                with self.assertRaises(FileExistsError):
                    self.helper("prepare")
                self.assertEqual(list(path.iterdir()), [marker])
                self.assertEqual(marker.read_text(), "test-token")
                marker.unlink()
                path.rmdir()

    def test_cleanup_refuses_missing_or_wrong_marker_before_signaling(self):
        self.helper("prepare")
        marker = Path(self.config["data_dir"], ".owner")
        for value in (None, "someone-else"):
            with self.subTest(marker=value):
                if value is None:
                    marker.unlink()
                else:
                    marker.write_text(value)
                with mock.patch.object(os, "killpg") as kill, \
                        self.assertRaises(RuntimeError):
                    self.helper("cleanup")
                kill.assert_not_called()
                self.assertTrue(Path(self.config["binary_dir"]).is_dir())
                self.assertTrue(Path(self.config["data_dir"]).is_dir())

    def test_cleanup_refuses_directory_and_marker_symlinks(self):
        target = self.root / "existing"
        target.mkdir()
        (target / ".owner").write_text("test-token")
        data = Path(self.config["data_dir"])
        data.symlink_to(target, target_is_directory=True)
        with self.assertRaises(RuntimeError):
            self.helper("cleanup")
        data.unlink()
        data.mkdir()
        (data / ".owner").symlink_to(target / ".owner")
        with self.assertRaises(RuntimeError):
            self.helper("cleanup")
        self.assertEqual((target / ".owner").read_text(), "test-token")

    def test_cleanup_does_not_signal_reused_pid(self):
        self.helper("prepare")
        Path(self.config["binary_dir"], "rsync.json").write_text(json.dumps(
            {"pid": os.getpid(), "start": "not-this-process-start-time"}))
        with mock.patch.object(os, "killpg") as kill:
            self.helper("cleanup")
        kill.assert_not_called()

    def test_expiry_reaps_receiver_but_never_removes_artifacts(self):
        self.helper("prepare")
        # Expiry must not remove even a replaced directory or an active rsync's files.
        Path(self.config["data_dir"], ".owner").write_text("someone-else")
        payload = Path(self.config["data_dir"], "payload")
        payload.write_text("still in use")
        rsync_state = Path(self.config["binary_dir"], "rsync.json")
        rsync_state.write_text("keep this record")
        child = mock.Mock(pid=os.getpid())
        child.poll.return_value = None
        child.wait.side_effect = [subprocess.TimeoutExpired("receiver", 1), 0]
        errors = io.StringIO()
        with mock.patch.object(sys, "argv", ["supervisor", json.dumps(self.config)]), \
                mock.patch.object(signal, "signal"), \
                mock.patch.object(subprocess, "Popen", return_value=child), \
                mock.patch.object(os, "killpg") as kill, \
                contextlib.redirect_stderr(errors):
            exec(compile(benchmark.SUPERVISOR, "<supervisor>", "exec"), {})
        kill.assert_called_once_with(child.pid, signal.SIGKILL)
        self.assertEqual(child.wait.call_count, 2)
        self.assertEqual(payload.read_text(), "still in use")
        self.assertEqual(rsync_state.read_text(), "keep this record")
        self.assertIn("artifacts retained", errors.getvalue())

    def test_buffer_size_endpoint_commands_and_report(self):
        source = self.root / "source"
        source.write_bytes(b"payload")
        binary = str(Path(sys.executable).resolve())
        rsync_commands = []
        for value in (None, 4096, 262144, 4194304):
            with self.subTest(buffer_size=value):
                output = self.root / ("reports-" + str(value))
                args = argparse.Namespace(host="unused", source=str(source),
                                          destination=str(self.root), gosync=binary,
                                          output=str(output), runs=1, warmups=0,
                                          timeout=5, buffer_size=value)
                receiver_commands = []
                configs = []

                def command(argv, timeout, input_text=None):
                    result = {"argv": argv, "returncode": 0, "timed_out": False,
                              "stdout": "test\n", "wall_seconds": 1}
                    if argv[0] == "ssh" and input_text == benchmark.REMOTE:
                        config = json.loads(shlex.split(argv[-1])[-1])
                        action = config["action"]
                        if action == "start":
                            configs.append(config)
                            child = mock.Mock(pid=os.getpid())
                            child.poll.return_value = 0
                            with mock.patch.object(sys, "argv", ["supervisor", json.dumps(config)]), \
                                    mock.patch.object(subprocess, "Popen", return_value=child) as popen:
                                exec(compile(benchmark.SUPERVISOR, "<supervisor>", "exec"), {})
                            receiver_commands.append(popen.call_args.args[0])
                            # The mock uses our PID only for /proc reads, not a real receiver.
                            Path(config["binary_dir"], "receiver.json").unlink()
                            reply = {"port": 12345}
                        elif action == "verify":
                            reply = {"size": 7, "sha256": "test"}
                        elif action in ("reset", "cpu"):
                            reply = {}
                        else:
                            reply = self.helper(action, config)
                        result["stdout"] = json.dumps(reply)
                    elif argv[:2] == ["ssh", "-G"]:
                        result["stdout"] = "hostname unused\n"
                    return result

                with mock.patch.object(benchmark, "parse_args", return_value=args), \
                        mock.patch.object(benchmark.shutil, "which", return_value="unused"), \
                        mock.patch.object(benchmark, "run_command", side_effect=command), \
                        mock.patch.object(signal, "signal"), \
                        contextlib.redirect_stdout(io.StringIO()):
                    self.assertEqual(benchmark.main(), 0)
                report = json.loads(next(output.glob("*.json")).read_text())
                self.assertEqual(report["arguments"]["buffer_size"], value)
                self.assertEqual(configs[0]["buffer_size"], value)
                flags = [] if value is None else ["--buffer-size", str(value)]
                self.assertEqual(receiver_commands, [[
                    configs[0]["binary_dir"] + "/gosync", "serve", "--listen",
                    "0.0.0.0:0", "--base", configs[0]["data_dir"], *flags]])
                gosync, rsync = report["trials"]
                self.assertEqual(gosync["argv"], [
                    binary, "-transport", "server", "-workers", "1", "-checksum",
                    "-progress=false", *flags, str(source), "unused:12345:/payload"])
                self.assertNotIn("buffer-size", shlex.join(rsync["argv"]))
                # Normalize the unique staging token to compare the entire rsync command.
                rsync_commands.append([arg.replace(configs[0]["token"], "TOKEN")
                                       for arg in rsync["argv"]])
                self.assertIn("cleanup", report)
                for key in ("binary_dir", "data_dir"):
                    self.assertFalse(Path(configs[0][key]).exists())
        self.assertTrue(all(argv == rsync_commands[0] for argv in rsync_commands))

    def test_lost_prepare_response_still_triggers_caller_cleanup(self):
        source = self.root / "source"
        source.write_bytes(b"payload")
        output = self.root / "reports"
        args = argparse.Namespace(host="unused", source=str(source),
                                  destination=str(self.root), gosync=sys.executable,
                                  output=str(output), runs=1, warmups=0, timeout=5,
                                  buffer_size=None)
        actions = []

        def command(argv, timeout, input_text=None):
            result = {"returncode": 0, "timed_out": False, "stdout": "test\n"}
            if argv[0] == "ssh" and input_text == benchmark.REMOTE:
                config = json.loads(shlex.split(argv[-1])[-1])
                action = config["action"]
                actions.append(action)
                if action == "prepare":
                    saved = json.loads(next(output.glob("*.json")).read_text())
                    for key in ("token", "binary_dir", "data_dir"):
                        self.assertEqual(saved["remote"][key], config[key])
                reply = self.helper(action, config)
                result["stdout"] = json.dumps(reply)
                if action == "prepare":
                    result.update(returncode=255, stdout="")
            elif argv[0] == "scp" or (argv[0] == "rsync" and "--version" not in argv):
                self.fail("no transfer should be attempted")
            return result

        with mock.patch.object(benchmark, "parse_args", return_value=args), \
                mock.patch.object(benchmark.shutil, "which", return_value="unused"), \
                mock.patch.object(benchmark, "run_command", side_effect=command), \
                mock.patch.object(signal, "signal"), \
                contextlib.redirect_stdout(io.StringIO()), \
                contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(benchmark.main(), 1)
        self.assertEqual(actions, ["discover", "prepare", "cleanup"])
        report = json.loads(next(output.glob("*.json")).read_text())
        self.assertIn("cleanup", report)
        self.assertEqual(report["status"], "failed")
        for key in ("binary_dir", "data_dir"):
            self.assertFalse(Path(report["remote"][key]).exists())


if __name__ == "__main__":
    unittest.main()
