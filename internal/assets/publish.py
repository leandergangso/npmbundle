#!/usr/bin/env python3

import argparse
import json
import shutil
import subprocess
import sys
import tarfile
from pathlib import Path
from dataclasses import dataclass
from time import sleep
from typing import TypeIs


@dataclass
class Error:
    error: str


@dataclass
class PackageInfo:
    path: Path
    name: str
    version: str


@dataclass
class Inspection:
    package: PackageInfo
    exists: bool
    is_latest: bool


MIN_PYTHON = (3, 13)
MIN_NPM = (10, 0)


def main() -> int:
    res = check_environment()
    if is_error(res):
        error(res.error)
        return 1
    python_version, npm_version = res

    args = parse_args()

    registry = args.registry or get_default_registry()

    res = find_packages(args.package_dir)
    if is_error(res):
        error(res.error)
        return 1
    packages = res

    log()
    log("npmbundle publisher")
    log()
    log("Environment:")
    log(f"  Python: {python_version:<12} OK")
    log(f"  npm:    {npm_version:<12} OK")
    log()
    log(f"==> Found {len(packages)} package(s)")
    log(f"==> Registry: {registry}")
    log()
    if not args.execute:
        log("==> Dry run: no packages will be published.")
        log("    Use --execute to publish.")
        log()
        sleep(1)  # give user time to read

    inspected: list[Inspection] = []
    total = len(packages)

    for current, package in enumerate(packages, 1):
        res = inspect_package(package, registry)
        if is_error(res):
            error(res.error)
            return 1

        # inspected.append((package, name, version, status, publish))
        inspected.append(res)
        if res.package.name and res.package.version:
            # FIX:
            # print_package_status(current, total, package, name, version, status)
            pass
    log()

    # invalid = [item for item in inspected if item[3].startswith("INVALID")]
    # if invalid:
    #    log(f"==> Inspection failed: {len(invalid)} " f"invalid package(s).")
    #    return 1

    # FIX:
    # to_publish = [item for item in inspected if item[4]]
    to_publish = [item for item in inspected]
    # skipped = [item for item in inspected if not item[4]]
    skipped = [item for item in inspected]

    if not args.execute:
        log()
        log(f"    Ready:   {len(to_publish)}")
        log(f"    Skipped: {len(skipped)}")
        log()
        log("Use --execute to publish.")
        return 0

    if not to_publish:
        log("==> Nothing to publish.")
        return 0

    log("==> Publishing")
    log(f"    Ready:   {len(to_publish)}")
    log(f"    Skipped: {len(skipped)}")
    log()

    return publish_packages(
        to_publish,
        registry,
        args.tag,
        len(skipped),
    )


def publish_packages(
    # packages: list[tuple[Path, str, str, bool, str, bool]],
    packages: list[Inspection],
    registry: str,
    tag: str | None,
    skipped_count: int,
) -> int:
    success_count = 0
    fail_count = 0
    failed = []

    for info in packages:
        pkg = info.package
        publish_tag = "latest" if info.is_latest else tag or "archive"
        log(f"{pkg.name}@{pkg.version}", end="    ")

        if npm_publish(pkg.path, registry, publish_tag):
            log(f"[OK]", end="")
            success_count += 1
        else:
            log(f"[FAIL]", end="")
            log()
            fail_count += 1
            failed.append(f"{pkg.name}@{pkg.version} ({pkg.name})")

        log()

    log()
    log("==> Publish Summary:")
    log(f"    Success: {success_count}")
    log(f"    Skipped: {skipped_count} (already existed)")
    log(f"    Failed:  {fail_count}")

    if failed:
        log()
        log("==> Failed Packages:")
        for package in failed:
            log(f"    {package}")
        return 1

    return 0


def inspect_package(package: Path, registry: str) -> Inspection | Error:
    res = read_package_metadata(package)
    if is_error(res):
        return res
    package_name, package_version = res

    exists, is_latest = npm_version_exists(package_name, package_version, registry)
    if exists:
        return package_name, package_version, "  EXISTS", False
    return package_name, package_version, "+ READY", True


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Publish an npmbundle package set to an npm registry.",
        usage="%(prog)s <PACKAGE_DIR> [REGISTRY] [OPTIONS]",
    )

    parser.add_argument(
        "package_dir",
        type=Path,
        help="directory containing .tgz or .tar.gz package bundles",
    )

    parser.add_argument(
        "registry",
        nargs="?",
        help="target npm registry URL (default: npm configured registry)",
    )

    parser.add_argument(
        "--tag",
        help="npm dist-tag to use (default: archive)",
    )

    parser.add_argument(
        "--execute",
        action="store_true",
        help="actually publish packages; without this flag only inspect",
    )

    if len(sys.argv) == 1:
        parser.print_help()
        sys.exit(0)

    return parser.parse_args()


def print_package_status(
    current: int,
    total: int,
    package: Path,
    name: str,
    version: str,
    status: str,
) -> None:
    if name and version:
        identity = f"{name}@{version}"
    else:
        identity = package.name

    width = len(str(total))
    log(f"[{current:>{width}}/{total}] " f"{identity:<46}  {status}")


def check_environment() -> tuple[str, str] | Error:
    if sys.version_info < MIN_PYTHON:
        return Error(
            f"Python {MIN_PYTHON[0]}.{MIN_PYTHON[1]} or newer is required (found {sys.version_info.major}.{sys.version_info.minor})"
        )

    python_version = (
        f"{sys.version_info.major}."
        f"{sys.version_info.minor}."
        f"{sys.version_info.micro}"
    )

    npm_version = get_npm_version()
    npm_major, npm_minor = map(
        int,
        npm_version.split(".")[:2],
    )

    if (npm_major, npm_minor) < MIN_NPM:
        return Error(
            f"npm {MIN_NPM[0]}.{MIN_NPM[1]} or newer is required (found {npm_version})"
        )

    return python_version, npm_version


def find_packages(directory: Path) -> list[Path] | Error:
    if not directory.exists():
        return Error(f"directory '{directory}' not found")

    if not directory.is_dir():
        return Error(f"'{directory}' is not a directory")

    packages = sorted(
        [
            *directory.glob("*.tgz"),
            *directory.glob("*.tar.gz"),
        ]
    )

    if not packages:
        return Error(f"no package archives found in '{directory}'")

    return packages


def read_package_metadata(path: Path) -> tuple[str, str] | Error:
    try:
        with tarfile.open(path, mode="r:gz") as archive:
            member = find_package_json(archive)

            if not member.isfile():
                return Error("package.json is not a regular file")

            file = archive.extractfile(member)

            if file is None:
                return Error("unable to read package.json")

            metadata = json.load(file)

    except tarfile.TarError as exc:
        return Error(f"invalid tar archive: {exc}")
    except json.JSONDecodeError as exc:
        return Error(f"invalid package.json: {exc}")

    name = metadata.get("name")
    version = metadata.get("version")

    if not isinstance(name, str) or not name:
        return Error("package.json has no valid 'name'")

    if not isinstance(version, str) or not version:
        return Error("package.json has no valid 'version'")

    return name, version


def find_package_json(archive: tarfile.TarFile) -> tarfile.TarInfo:
    candidates = []

    for member in archive.getmembers():
        path = Path(member.name)

        if path.name != "package.json":
            continue

        # package.json must be exactly one directory below the
        # archive root.
        if len(path.parts) != 2:
            continue

        candidates.append(member)

    if len(candidates) != 1:
        raise ValueError("expected exactly one top-level package.json")

    return candidates[0]


def npm_version_exists(
    name: str, version: str, registry: str
) -> tuple[bool, bool] | Error:
    result = run_command(
        [
            "npm",
            "view",
            f"{name}@{version}",
            "version",
            "versions",
            "--json",
            "--fetch-retries=0",
            f"--registry={registry}",
        ],
        capture_output=True,
    )

    if result.returncode != 0:
        if "E404" in result.stderr:
            return False, False
        return Error(result.stderr)

    version_output = result.stdout.strip()
    if not version_output:
        return False, False

    # TODO: check if latest tag

    return True, False


def npm_publish(package: Path, registry: str, tag: str) -> bool:
    command = [
        "npm",
        "publish",
        f"./{package}",
        f"--registry={registry}",
        "--fetch-retries=0",
        "--ignore-scripts",
        "--provenance=false",
        "--silent",
        "--tag",
        tag,
    ]
    result = run_command(command)
    return result.returncode == 0


def get_default_registry() -> str:
    result = run_command(
        ["npm", "config", "get", "registry"],
        capture_output=True,
    )

    if result.returncode != 0 or not result.stdout.strip():
        return "https://registry.npmjs.org/"

    return result.stdout.strip()


def get_npm_version() -> str:
    result = run_command(
        ["npm", "--version"],
        capture_output=True,
    )

    if result.returncode != 0:
        error("unable to determine npm version")
        if result.stderr:
            log(result.stderr)
        sys.exit(1)

    version = result.stdout.strip()
    if not version:
        error("npm returned no version")
        sys.exit(1)

    return version


def run_command(
    args: list[str],
    *,
    capture_output: bool = False,
) -> subprocess.CompletedProcess[str]:
    try:
        return subprocess.run(
            args,
            check=False,
            text=True,
            capture_output=capture_output,
        )
    except OSError as exc:
        error(f"running command {exc}")
        sys.exit(1)


def command_exists(name: str) -> bool:
    return shutil.which(name) is not None


def log(message: str = "", end="\n") -> None:
    print(message, end=end, file=sys.stderr)


def error(message: str = "") -> None:
    print(f"Error: {message}", file=sys.stderr)


def is_error(value) -> TypeIs[Error]:
    return isinstance(value, Error)


if __name__ == "__main__":
    if not command_exists("npm"):
        error("npm command not found")
        sys.exit(1)

    try:
        sys.exit(main())
    except KeyboardInterrupt:
        log("\nInterrupted")
        sys.exit(130)
