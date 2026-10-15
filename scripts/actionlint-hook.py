#!/usr/bin/env python3
import fcntl
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time

ACTIONLINT_MODULE = "github.com/rhysd/actionlint"
ACTIONLINT_VERSION = "v1.7.12"
ACTIONLINT_SUM = "h1:vQ4GeJN86C0QH+gTUQcs8McmK62OLT3kmakPMtEWYnY="
LOCK_TIMEOUT_SECONDS = 600


def main() -> int:
    tools = Path(sys.executable).parent
    versions = {"shellcheck": "0.11.0", "pyflakes": "3.4.0"}
    for checker, expected in versions.items():
        binary = tools / checker
        if not binary.is_file() or not os.access(binary, os.X_OK):
            print(f"actionlint: missing required hook dependency {checker}; rebuild the pre-commit environment", file=sys.stderr)
            return 2
        try:
            result = subprocess.run(
                [str(binary), "--version"], check=True, capture_output=True,
                text=True, timeout=10,
            )
        except (OSError, subprocess.SubprocessError) as error:
            print(f"actionlint: cannot verify {checker}: {error}", file=sys.stderr)
            return 2
        actual = result.stdout.splitlines()
        valid = f"version: {expected}" in actual if checker == "shellcheck" else result.stdout.startswith(f"{expected} ")
        if not valid:
            print(f"actionlint: required {checker} {expected}; rebuild the pre-commit environment", file=sys.stderr)
            return 2
    go = shutil.which("go")
    if go is None:
        print("actionlint: Go is required to run pinned actionlint v1.7.12; install the project Go toolchain", file=sys.stderr)
        return 2
    actionlint = tools / "actionlint"
    try:
        with (tools / ".actionlint-install.lock").open("a") as lock:
            deadline = time.monotonic() + LOCK_TIMEOUT_SECONDS
            while True:
                try:
                    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    break
                except BlockingIOError:
                    if time.monotonic() >= deadline:
                        print("actionlint: timed out waiting for bootstrap lock; retry after the other hook process finishes", file=sys.stderr)
                        return 2
                    time.sleep(0.1)
            if actionlint.is_symlink():
                print("actionlint: cached actionlint must not be a symlink; rebuild the pre-commit environment", file=sys.stderr)
                return 2
            if not actionlint.exists():
                subprocess.run(
                    [go, "install", f"{ACTIONLINT_MODULE}/cmd/actionlint@{ACTIONLINT_VERSION}"],
                    env={**os.environ, "GOBIN": str(tools)}, check=True, timeout=300,
                )
            info = json.loads(subprocess.check_output(
                [go, "version", "-m", "-json", str(actionlint)], text=True, timeout=10,
            ))
            expected_module = {"Path": ACTIONLINT_MODULE, "Version": ACTIONLINT_VERSION, "Sum": ACTIONLINT_SUM}
            if (
                info.get("Path") != f"{ACTIONLINT_MODULE}/cmd/actionlint"
                or info.get("Main") != expected_module
                or not os.access(actionlint, os.X_OK)
            ):
                print("actionlint: cached binary does not match the pinned module; rebuild the pre-commit environment", file=sys.stderr)
                return 2
    except (OSError, subprocess.SubprocessError, ValueError) as error:
        print(f"actionlint: cannot provision or verify pinned actionlint: {error}", file=sys.stderr)
        return 2
    os.execv(str(actionlint), [
        str(actionlint),
        "-shellcheck", str(tools / "shellcheck"),
        "-pyflakes", str(tools / "pyflakes"), *sys.argv[1:],
    ])


if __name__ == "__main__":
    sys.exit(main())
