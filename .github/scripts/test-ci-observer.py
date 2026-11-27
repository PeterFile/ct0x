#!/usr/bin/env python3
"""Run every offline observer suite from any working directory; no private setup."""
import argparse
from pathlib import Path
import subprocess
import sys
import unittest

sys.dont_write_bytecode = True


def main(argv=None):
    parser = argparse.ArgumentParser(description="运行 CI 观察器 Python 与 Node 离线测试")
    parser.add_argument("-v", "--verbose", action="store_true")
    args = parser.parse_args(argv)
    directory = Path(__file__).resolve().parent / "observer"
    suite = unittest.defaultTestLoader.discover(str(directory), pattern="test_*.py")
    python_ok = unittest.TextTestRunner(verbosity=2 if args.verbose else 1).run(suite).wasSuccessful()
    files = sorted(str(p) for p in directory.glob("test-pi-receiver.*") if p.suffix in {".mjs", ".cjs", ".js"})
    if not files:
        print("缺少 Pi 接收器离线测试，不能宣称完整通过", file=sys.stderr)
        return 1
    try:
        node_ok = subprocess.run(["node", "--test", *files], cwd=directory.parents[2]).returncode == 0
    except OSError:
        print("Node 测试无法启动；请检查本机 Node，不自动安装", file=sys.stderr)
        node_ok = False
    return 0 if python_ok and node_ok else 1


if __name__ == "__main__":
    sys.exit(main())
