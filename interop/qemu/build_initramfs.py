#!/usr/bin/env python3
"""Build an isolated native-Homa test guest without host root privileges."""
import argparse
import gzip
import hashlib
import json
import lzma
import os
from pathlib import Path
import platform
import re
import shutil
import stat
import subprocess
import tempfile


def run(*args):
    return subprocess.check_output(args, text=True).strip()


def cpio_entry(name, data, mode, inode, rmajor=0, rminor=0):
    """Write newc directly so dev/console needs no privileged host mknod."""
    encoded = name.encode() + b"\0"
    fields = [inode, mode, 0, 0, 1, 0, len(data), 0, 0, rmajor, rminor,
              len(encoded), 0]
    header = b"070701" + b"".join(f"{value:08x}".encode() for value in fields)
    entry = header + encoded
    entry += b"\0" * (-len(entry) % 4)
    entry += data
    return entry + b"\0" * (-len(data) % 4)


def dependencies(binary, root):
    text = run("ldd", str(binary))
    if "not found" in text:
        raise RuntimeError(f"Missing binary dependency: {text}")
    for path in re.findall(r"(?:=>\s+)?(/[^\s]+)", text):
        source = Path(path)
        dest = root / source.relative_to("/")
        dest.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source, dest, follow_symlinks=True)


def sha256(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--module", type=Path, required=True, help="Built pinned homa.ko")
    parser.add_argument("--out-dir", type=Path, required=True)
    parser.add_argument("--kernel-release", default=platform.release())
    parser.add_argument("--cc", default="cc")
    args = parser.parse_args()
    source = Path(__file__).resolve().parent
    args.module = args.module.resolve(strict=True)
    args.out_dir = args.out_dir.resolve()
    args.out_dir.mkdir(parents=True, exist_ok=True)
    ethernet = Path("/lib/modules") / args.kernel_release / "kernel/drivers/net/ethernet/intel/e1000/e1000.ko.xz"
    if not ethernet.exists():
        ethernet = ethernet.with_suffix("")
    if not ethernet.exists():
        raise RuntimeError("Need the matching e1000.ko[.xz]; no modules are installed by this script")
    for module in [args.module, ethernet]:
        deps = run("modinfo", "-F", "depends", str(module))
        if deps:
            raise RuntimeError(f"Additional modules must be packaged explicitly for {module}: {deps}")
        vermagic = run("modinfo", "-F", "vermagic", str(module))
        if vermagic.split()[0] != args.kernel_release:
            raise RuntimeError(f"Kernel version mismatch for {module}: {vermagic}")
    with tempfile.TemporaryDirectory(prefix="homa-guest-", dir=args.out_dir) as tmp:
        root = Path(tmp)
        for name in ["dev", "proc", "sys", "tmp", "run"]:
            (root / name).mkdir()
        common = [args.cc, "-std=c11", "-O2", "-Wall", "-Wextra", "-Werror", "-pedantic"]
        subprocess.check_call([*common, "-o", str(root / "init"), str(source / "guest_init.c")])
        subprocess.check_call([*common, "-o", str(root / "linux-peer"), str(source.parent / "linux_peer.c")])
        dependencies(root / "init", root)
        dependencies(root / "linux-peer", root)
        shutil.copy2(args.module, root / "homa.ko")
        netdata = ethernet.read_bytes()
        if ethernet.suffix == ".xz":
            netdata = lzma.decompress(netdata)
        (root / "e1000.ko").write_bytes(netdata)
        archive = bytearray()
        manifest = {}
        inode = 1
        for path in sorted(root.rglob("*")):
            relative = str(path.relative_to(root))
            if path.is_dir():
                mode, data = stat.S_IFDIR | 0o755, b""
            else:
                mode = stat.S_IFREG | (0o755 if relative in ["init", "linux-peer"] or os.access(path, os.X_OK) else 0o644)
                data = path.read_bytes()
                manifest[relative] = {"sha256": hashlib.sha256(data).hexdigest(), "bytes": len(data)}
            archive += cpio_entry(relative, data, mode, inode)
            inode += 1
        archive += cpio_entry("dev/console", b"", stat.S_IFCHR | 0o600, inode, 5, 1)
        archive += cpio_entry("TRAILER!!!", b"", 0, inode + 1)
        archive += b"\0" * (-len(archive) % 512)
        output = args.out_dir / "homa-initramfs.cpio.gz"
        with output.open("wb") as handle:
            with gzip.GzipFile(fileobj=handle, mode="wb", mtime=0) as zipped:
                zipped.write(archive)
        report = {"kernel_release": args.kernel_release, "upstream_commit": "d8914b8a57aa48c19a2c8130484961585bcbe53c",
                  "module": str(args.module), "module_sha256": sha256(args.module),
                  "initramfs_sha256": sha256(output), "files": manifest}
        (args.out_dir / "manifest.json").write_text(json.dumps(report, indent=2) + "\n")
        print(json.dumps({"initramfs": str(output), "bytes": output.stat().st_size,
                          "manifest": str(args.out_dir / "manifest.json")}, indent=2))


if __name__ == "__main__":
    main()
