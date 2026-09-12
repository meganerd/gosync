#!/usr/bin/env python3
"""Bounded, warm-cache gosync TCP versus rsync/SSH benchmark (Linux remote)."""
import argparse
import json
import math
import os
from pathlib import Path
import platform
import re
import resource
import shlex
import shutil
import signal
import statistics
import subprocess
import sys
import time
import uuid

SSH_OPTIONS = ["-o", "BatchMode=yes", "-o", "Compression=no", "-o", "ConnectTimeout=10",
               "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2"]

# This wrapper bounds the remote rsync even if its SSH client disappears.
RSYNC_WRAPPER = r'''
import json, os, signal, subprocess, sys
from pathlib import Path
signal.signal(signal.SIGHUP, signal.SIG_IGN)
def terminate(signum, frame):
    raise SystemExit(128 + signum)
signal.signal(signal.SIGTERM, terminate)
p = subprocess.Popen(["rsync"] + sys.argv[2:], start_new_session=True)
try:
    fields = Path("/proc/%d/stat" % p.pid).read_text().rsplit(")", 1)[1].split()
    Path(__file__).with_name("rsync.json").write_text(json.dumps(
        {"pid": p.pid, "start": fields[19]}))
    code = p.wait(timeout=float(sys.argv[1]))
except BaseException:
    if p.poll() is None:
        os.killpg(p.pid, signal.SIGKILL)
    p.wait(timeout=5)
    raise
sys.exit(code)
'''

# Parent remains alive to reap the receiver and impose a hard lifetime limit.
SUPERVISOR = r'''
import json, os, signal, subprocess, sys
c = json.loads(sys.argv[1])
def terminate(signum, frame):
    raise SystemExit(128 + signum)
signal.signal(signal.SIGTERM, terminate)
p = None
expired = False
try:
    with open(c["binary_dir"] + "/receiver.log", "ab", buffering=0) as log:
        command = [c["binary_dir"] + "/gosync", "serve", "--listen",
                   "0.0.0.0:0", "--base", c["data_dir"]]
        if c.get("buffer_size") is not None:
            command += ["--buffer-size", str(c["buffer_size"])]
        p = subprocess.Popen(command,
                             stdin=subprocess.DEVNULL, stdout=log, stderr=log,
                             start_new_session=True)
        with open("/proc/%d/stat" % p.pid) as proc_stat:
            stat = proc_stat.read().rsplit(")", 1)[1].split()
        state = {"pid": p.pid, "start": stat[19]}
        path = c["binary_dir"] + "/receiver.json"
        with open(path + ".tmp", "w") as f:
            json.dump(state, f)
        os.replace(path + ".tmp", path)
        try:
            p.wait(timeout=c["lifetime"])
        except subprocess.TimeoutExpired:
            expired = True
finally:
    if p is not None and p.poll() is None:
        os.killpg(p.pid, signal.SIGKILL)
        p.wait(timeout=5)
    if expired:
        print("Receiver lifetime expired; artifacts retained for ownership-checked cleanup: "
              + c["data_dir"] + " and " + c["binary_dir"], file=sys.stderr, flush=True)
'''

REMOTE = r'''
import hashlib, json, os, platform, shutil, signal, socket, subprocess, sys, time
from pathlib import Path
c = json.loads(sys.argv[1])
action = c["action"]
def deadline(signum, frame):
    raise TimeoutError("remote helper deadline exceeded")
signal.signal(signal.SIGALRM, deadline)
signal.setitimer(signal.ITIMER_REAL, c["helper_timeout"])

def proc(state):
    try:
        fields = Path("/proc/%d/stat" % state["pid"]).read_text().rsplit(")", 1)[1].split()
        if fields[19] != state["start"] or fields[0] == "Z":
            return None
        return {"user_seconds": int(fields[11]) / os.sysconf("SC_CLK_TCK"),
                "system_seconds": int(fields[12]) / os.sysconf("SC_CLK_TCK")}
    except FileNotFoundError:
        return None

def state():
    return json.loads(Path(c["binary_dir"], "receiver.json").read_text())

def owned():
    for key in ("data_dir", "binary_dir"):
        p = Path(c[key])
        if not os.path.lexists(p):
            continue
        marker = p / ".owner"
        if p.is_symlink() or not p.is_dir() or marker.is_symlink() or not marker.is_file():
            raise RuntimeError("missing or unsafe ownership marker: " + str(p))
        if marker.read_text() != c["token"]:
            raise RuntimeError("ownership marker mismatch: " + str(p))

def stop():
    # Stop the supervisor first so it cannot launch a receiver during cleanup.
    for filename, sig in (("rsync.json", signal.SIGKILL),
                              ("supervisor.json", signal.SIGTERM), ("receiver.json", signal.SIGKILL)):
        try:
            s = json.loads(Path(c["binary_dir"], filename).read_text())
        except FileNotFoundError:
            continue
        if proc(s) is not None:
            try:
                os.killpg(s["pid"], sig)
            except ProcessLookupError:
                continue
            end = time.monotonic() + 5
            while proc(s) is not None and time.monotonic() < end:
                time.sleep(.05)
            if proc(s) is not None:
                raise RuntimeError("owned process did not exit; refusing directory removal")

def read_optional(path):
    try:
        return Path(path).read_text()
    except OSError as e:
        return str(e)

result = {}
if action in ("discover", "prepare"):
    dest = Path(c["destination"]).resolve(strict=True)
    if not dest.is_dir() or not os.access(dest, os.W_OK | os.X_OK):
        raise RuntimeError("destination must be an existing writable directory")
    home = Path.home().resolve(strict=True)
    if action == "discover":
        print(json.dumps({"home": str(home), "destination": str(dest)}))
        sys.exit(0)
    if shutil.disk_usage(dest).free < c["size"] + c["reserve"]:
        raise RuntimeError("insufficient destination free space")
    if shutil.disk_usage(home).free < c["binary_size"] + (16 << 20):
        raise RuntimeError("insufficient HOME staging space")
    if not shutil.which("rsync"):
        raise RuntimeError("remote rsync not found")
    binary, data = c["binary_dir"], c["data_dir"]
    for d in (binary, data):
        if os.path.lexists(d):
            raise FileExistsError("refusing existing benchmark path: " + d)
    # Never adopt an existing path, even if it already has a matching marker.
    # The caller knows both paths and attempts cleanup if this response is lost.
    for d in (binary, data):
        Path(d).mkdir(mode=0o700)
        with Path(d, ".owner").open("x") as marker:
            marker.write(c["token"])
    Path(binary, "supervisor.py").write_text(c["supervisor"])
    Path(binary, "rsync.py").write_text(c["rsync_wrapper"])
    result = {"binary_dir": binary, "data_dir": data,
              "environment": {"platform": platform.platform(),
                "python": sys.version, "cpu_count": os.cpu_count(),
                "cpuinfo": read_optional("/proc/cpuinfo"),
                "meminfo": read_optional("/proc/meminfo"),
                "mountinfo": read_optional("/proc/self/mountinfo"),
                "destination_free_bytes": shutil.disk_usage(dest).free,
                "statvfs": list(os.statvfs(dest)),
                "rsync_version": subprocess.check_output(["rsync", "--version"],
                                                          timeout=10, text=True)}}
else:
    owned()
    if action == "start":
        binary = c["binary_dir"] + "/gosync"
        os.chmod(binary, 0o700)
        result["version"] = subprocess.check_output([binary, "-version"], timeout=10, text=True)
        with open(c["binary_dir"] + "/supervisor.log", "ab") as log:
            supervisor = subprocess.Popen([sys.executable, c["binary_dir"] + "/supervisor.py",
                                           json.dumps(c)], stdin=subprocess.DEVNULL,
                                          stdout=log, stderr=log, start_new_session=True)
        fields = Path("/proc/%d/stat" % supervisor.pid).read_text().rsplit(")", 1)[1].split()
        Path(c["binary_dir"], "supervisor.json").write_text(json.dumps(
            {"pid": supervisor.pid, "start": fields[19]}))
        result["supervisor_pid"] = supervisor.pid
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            try:
                s = state()
                if proc(s) is None:
                    raise RuntimeError("receiver exited: " + read_optional(c["binary_dir"] + "/receiver.log"))
                # Discover the actual port from this process's listening socket, not a port reservation race.
                inodes = set()
                for fd in Path("/proc/%d/fd" % s["pid"]).iterdir():
                    try:
                        link = os.readlink(fd)
                    except FileNotFoundError:
                        continue
                    if link.startswith("socket:["):
                        inodes.add(link[8:-1])
                for table in ("/proc/net/tcp", "/proc/net/tcp6"):
                    for line in Path(table).read_text().splitlines()[1:]:
                        f = line.split()
                        if f[3] == "0A" and f[9] in inodes:
                            port = int(f[1].split(":")[1], 16)
                            with socket.create_connection(("127.0.0.1", port), timeout=1):
                                pass
                            result.update(s, port=port)
                            break
                    if "port" in result:
                        break
                if "port" in result:
                    break
            except FileNotFoundError:
                pass
            time.sleep(.1)
        else:
            stop()
            raise RuntimeError("receiver readiness timed out")
    elif action == "reset":
        if proc(state()) is None:
            raise RuntimeError("receiver is no longer alive")
        # Only the owned trial payload is ever removed between trials.
        Path(c["data_dir"], "payload").unlink(missing_ok=True)
        if shutil.disk_usage(c["data_dir"]).free < c["size"] + c["reserve"]:
            raise RuntimeError("insufficient free space before trial")
    elif action == "cpu":
        result = proc(state())
    elif action == "verify":
        p = Path(c["data_dir"], "payload")
        h = hashlib.sha256()
        with p.open("rb") as f:
            for block in iter(lambda: f.read(8 << 20), b""):
                h.update(block)
        result = {"size": p.stat().st_size, "sha256": h.hexdigest()}
    elif action == "cleanup":
        stop()
        result = {"receiver_log": read_optional(c["binary_dir"] + "/receiver.log"),
                  "supervisor_log": read_optional(c["binary_dir"] + "/supervisor.log")}
        owned()
        for key in ("data_dir", "binary_dir"):
            if Path(c[key]).exists():
                shutil.rmtree(c[key])
    else:
        raise RuntimeError("unknown action")
print(json.dumps(result))
'''


def run_command(argv, timeout, input_text=None):
    """Time only the child command; always reap and kill its remaining process group."""
    before = resource.getrusage(resource.RUSAGE_CHILDREN)
    start = time.perf_counter()
    p = subprocess.Popen(argv, stdin=subprocess.PIPE if input_text is not None else subprocess.DEVNULL,
                         stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
    timed_out = False
    try:
        try:
            out, err = p.communicate(None if input_text is None else input_text.encode(), timeout=timeout)
        except subprocess.TimeoutExpired:
            timed_out = True
            os.killpg(p.pid, signal.SIGKILL)
            out, err = p.communicate(timeout=5)
    finally:
        try:
            os.killpg(p.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        p.wait(timeout=5)
    elapsed = time.perf_counter() - start
    after = resource.getrusage(resource.RUSAGE_CHILDREN)
    return {"argv": argv, "returncode": p.returncode, "timed_out": timed_out,
            "wall_seconds": elapsed, "local_child_user_seconds": after.ru_utime - before.ru_utime,
            "local_child_system_seconds": after.ru_stime - before.ru_stime,
            "stdout": out.decode("utf-8", "replace"), "stderr": err.decode("utf-8", "replace")}


def checked(result):
    if result["returncode"] or result["timed_out"]:
        raise RuntimeError("command failed: " + json.dumps(result))
    return result["stdout"]


def positive_int(value):
    n = int(value)
    if n < 1:
        raise argparse.ArgumentTypeError("must be positive")
    return n


def buffer_size(value):
    n = int(value)
    if not 4096 <= n <= 4194304:
        raise argparse.ArgumentTypeError("must be between 4096 and 4194304 bytes")
    return n


def parse_args():
    p = argparse.ArgumentParser(description=__doc__)
    for name in ("host", "source", "destination", "gosync", "output"):
        p.add_argument("--" + name, required=True)
    p.add_argument("--runs", type=positive_int, default=3, help="measured rounds (default: 3)")
    p.add_argument("--warmups", type=int, default=1, help="warmup rounds, one trial per tool (default: 1)")
    p.add_argument("--timeout", type=float, default=120, help="seconds per command/hash (default: 120)")
    p.add_argument("--buffer-size", type=buffer_size,
                   help="gosync buffer bytes, 4096..4194304 (default: omit flag for older binaries)")
    args = p.parse_args()
    if args.warmups < 0 or not math.isfinite(args.timeout) or args.timeout <= 0:
        p.error("warmups must be nonnegative and timeout finite and positive")
    if not re.fullmatch(r"(?:[A-Za-z0-9_][A-Za-z0-9_.-]*@)?[A-Za-z0-9_][A-Za-z0-9_.-]*", args.host):
        p.error("host must be a simple SSH hostname/alias, optionally user@host (no IPv6)")
    if not args.destination.startswith("/") or "\n" in args.destination or "\x00" in args.destination:
        p.error("destination must be an absolute remote directory without newline/NUL")
    return args


def main():
    args = parse_args()
    source = Path(args.source).resolve(strict=True)
    binary = Path(args.gosync).resolve(strict=True)
    if not source.is_file() or source.stat().st_size <= 0 or not os.access(source, os.R_OK):
        raise ValueError("source must be a nonempty readable regular file")
    if not binary.is_file() or not os.access(binary, os.X_OK):
        raise ValueError("gosync must be an executable local regular file")
    for tool in ("ssh", "scp", "rsync"):
        if not shutil.which(tool):
            raise ValueError(tool + " is required locally")
    output = Path(args.output).resolve()
    output.mkdir(parents=True, exist_ok=True)
    token = uuid.uuid4().hex
    report_path = output / ("benchmark-transfer-" + token + ".json")
    size = source.stat().st_size
    original_stat = source.stat()
    config = {"token": token, "destination": args.destination, "size": size,
              "binary_size": binary.stat().st_size, "reserve": 64 << 20,
              "buffer_size": args.buffer_size,
                            "helper_timeout": args.timeout,
              "lifetime": (args.runs + args.warmups) * 2 * (6 * args.timeout + 30) + 120}
    report = {"schema_version": 1, "arguments": vars(args), "source_bytes": size,
              "started_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
              "trials": [], "operations": [], "summary": {}, "status": "running",
              "local_environment": {"platform": platform.platform(), "python": sys.version,
                                    "cpu_count": os.cpu_count(),
                                    "source_filesystem": list(os.statvfs(source)),
                                    "source_free_bytes": shutil.disk_usage(source).free}}

    def save():
        temporary = report_path.with_suffix(".tmp")
        temporary.write_text(json.dumps(report, indent=2) + "\n")
        temporary.replace(report_path)

    def remote(action, **extra):
        c = dict(config, action=action, **extra)
        command = shlex.join(["python3", "-", json.dumps(c)])
        result = run_command(["ssh", *SSH_OPTIONS, args.host, command], args.timeout, REMOTE)
        report["operations"].append({"action": action, **result})
        return json.loads(checked(result))

    def cpu():
        try:
            return remote("cpu")
        except Exception as e:
            return {"unavailable": str(e)}

    def interrupted(signum, frame):
        raise KeyboardInterrupt("signal %d" % signum)

    signal.signal(signal.SIGTERM, interrupted)
    cleanup_needed = False
    try:
        versions = {}
        for name, argv in (("gosync", [str(binary), "-version"]), ("rsync", ["rsync", "--version"]),
                           ("ssh", ["ssh", "-V"])):
            result = run_command(argv, args.timeout)
            report["operations"].append({"action": name + "_version", **result})
            checked(result)
            versions[name] = result
        report["local_environment"]["versions"] = versions
        # Hash in a child so even a blocked filesystem read has a command deadline.
        hash_script = """import hashlib, sys
h = hashlib.sha256()
with open(sys.argv[1], 'rb') as f:
    for block in iter(lambda: f.read(8 << 20), b''):
        h.update(block)
print(h.hexdigest())
"""
        result = run_command([sys.executable, "-c", hash_script, str(source)], args.timeout)
        report["operations"].append({"action": "warm_source_sha256", **result})
        digest = checked(result).strip()
        report["source_sha256"] = digest
        locations = remote("discover")
        config["destination"] = locations["destination"]
        config["binary_dir"] = str(Path(locations["home"]) / ("gosync-bench-" + token + "-bin"))
        config["data_dir"] = str(Path(locations["destination"]) / ("gosync-bench-" + token + "-data"))
        report["remote"] = {key: config[key] for key in ("token", "binary_dir", "data_dir")}
        save()
        cleanup_needed = True
        info = remote("prepare", supervisor=SUPERVISOR, rsync_wrapper=RSYNC_WRAPPER)
        report["remote"].update(info)
        save()
        # Legacy SCP's remote shell requires quoting; also works with OpenSSH SFTP for simple HOME paths.
        stage = config["binary_dir"] + "/gosync"
        if not re.fullmatch(r"/[A-Za-z0-9_./-]+", stage):
            raise ValueError("remote HOME staging path must contain only simple pathname characters for scp")
        result = run_command(["scp", "-q", *SSH_OPTIONS, str(binary), args.host + ":" + stage], args.timeout)
        report["operations"].append({"action": "stage_binary", **result})
        checked(result)
        receiver = remote("start")
        report["receiver"] = receiver
        ssh_config = run_command(["ssh", "-G", *SSH_OPTIONS, args.host], args.timeout)
        report["operations"].append({"action": "ssh_config", **ssh_config})
        hostname = next(line.split(None, 1)[1] for line in checked(ssh_config).splitlines()
                        if line.startswith("hostname "))
        if not re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_.-]*", hostname):
            raise ValueError("resolved SSH hostname must be an IPv4 address or DNS name")
        rsync_path = shlex.join(["python3", config["binary_dir"] + "/rsync.py", str(args.timeout)])
        commands = {
            "gosync": [str(binary), "-transport", "server", "-workers", "1", "-checksum",
                       "-progress=false",
                       *([] if args.buffer_size is None else ["--buffer-size", str(args.buffer_size)]),
                       str(source), "%s:%d:/payload" % (hostname, receiver["port"])],
            "rsync": ["rsync", "-rt", "--whole-file", "--ignore-times", "--no-compress", "--stats",
                      "--protect-args", "--rsync-path=" + rsync_path,
                      "-e", shlex.join(["ssh", *SSH_OPTIONS]), str(source),
                      args.host + ":" + config["data_dir"] + "/payload"]}
        for phase, rounds in (("warmup", args.warmups), ("measured", args.runs)):
            for round_index in range(rounds):
                order = ("gosync", "rsync") if round_index % 2 == 0 else ("rsync", "gosync")
                for tool in order:
                    remote("reset")
                    before_cpu = cpu() if tool == "gosync" else None
                    trial = {"tool": tool, "phase": phase, "round": round_index + 1}
                    report["trials"].append(trial)
                    print(f"{phase} {round_index + 1}/{rounds}: {tool}", flush=True)
                    trial.update(run_command(commands[tool], args.timeout))
                    after_cpu = cpu() if tool == "gosync" else None
                    trial["remote_receiver_cpu_before"] = before_cpu
                    trial["remote_receiver_cpu_after"] = after_cpu
                    if before_cpu and after_cpu and "user_seconds" in before_cpu and "user_seconds" in after_cpu:
                        trial["remote_receiver_cpu_delta"] = {k: after_cpu[k] - before_cpu[k] for k in before_cpu}
                    # Attempt verification even after a failed or timed-out transfer, never inside its timer.
                    try:
                        trial["verification"] = remote("verify")
                        trial["verified"] = trial["verification"] == {"size": size, "sha256": digest}
                    except Exception as e:
                        trial["verified"] = False
                        trial["verification_error"] = str(e)
                    save()
                    checked(trial)
                    if not trial["verified"]:
                        raise RuntimeError("SHA256/size verification failed")
                    current = source.stat()
                    if (current.st_size, current.st_mtime_ns, current.st_ino) != (
                            original_stat.st_size, original_stat.st_mtime_ns, original_stat.st_ino):
                        raise RuntimeError("source changed during benchmark")
                    trial["MB_per_second"] = size / trial["wall_seconds"] / 1_000_000
                    trial["MiB_per_second"] = size / trial["wall_seconds"] / (1 << 20)
                    print(f"  {trial['wall_seconds']:.3f}s, {trial['MB_per_second']:.2f} MB/s, "
                          f"{trial['MiB_per_second']:.2f} MiB/s; SHA-256 verified", flush=True)
                    save()
        for tool in commands:
            trials = [t for t in report["trials"] if t["tool"] == tool and t["phase"] == "measured"]
            report["summary"][tool] = {"samples": len(trials), **{
                "median_" + key: statistics.median(t[key] for t in trials)
                for key in ("wall_seconds", "MB_per_second", "MiB_per_second")}}
        report["status"] = "complete"
    except (Exception, KeyboardInterrupt) as e:
        report["status"] = "failed"
        report["error"] = str(e)
    finally:
        # Do not let a second Ctrl-C interrupt the bounded cleanup attempt.
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        if cleanup_needed:
            try:
                report["cleanup"] = remote("cleanup")
            except Exception as e:
                report["cleanup_error"] = str(e)
                report["status"] = "failed"
        save()
    print(str(report_path))
    print(json.dumps(report["summary"], indent=2))
    if report["status"] != "complete":
        print(report.get("error", report.get("cleanup_error", "failed")), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError) as error:
        print("benchmark: " + str(error), file=sys.stderr)
        sys.exit(1)
