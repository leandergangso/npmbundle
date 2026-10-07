#!/usr/bin/env python3

import argparse
import json
import shutil
import subprocess
import sys
import tarfile
from pathlib import Path
from typing import TypeIs
from dataclasses import dataclass


@dataclass(frozen=True)
class Error:
    error: str


@dataclass(frozen=True)
class PackageInfo:
    path: Path
    name: str
    version: str


@dataclass(frozen=True)
class Inspection:
    package: PackageInfo
    exists: bool
    is_latest: bool


@dataclass(frozen=True)
class SemVer:
    major: int
    minor: int
    patch: int
    prerelease: bool

    @classmethod
    def parse(cls, value: str) -> SemVer:
        """
        parse npm package versions, we assume the version is already valid semver.
        prereleases should not have latest tag so simplifies this logic.
        """
        core = value.split("+", 1)[0]
        core, separator, _ = core.partition("-")
        major, minor, patch = core.split(".")
        return cls(
            major=int(major),
            minor=int(minor),
            patch=int(patch),
            prerelease=bool(separator),
        )


MIN_PYTHON = (3, 13)
MIN_NPM = (10, 0)


def main() -> int:
    args = parse_args()

    if not command_exists("npm"):
        error("npm command not found")
        return 1

    info = check_environment()
    if is_error(info):
        error(info.error)
        return 1

    python_version, npm_version = info

    registry = get_registry(args.registry)
    if is_error(registry):
        error(registry.error)
        return 1

    packages = find_packages(args.package_dir)
    if is_error(packages):
        error(packages.error)
        return 1

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

    if args.dry_run:
        log("==> Dry run: no packages will be published")
        log()

    total = len(packages)
    inspected: list[Inspection] = []

    for current, package in enumerate(packages, 1):
        info = inspect_package(package, registry)
        if is_error(info):
            error(f"{package.name}: {info.error}")
            return 1

        inspected.append(info)

        status = "EXISTS" if info.exists else "+ READY"
        print_package_status(current, total, info, status)

    log()

    to_publish = [item for item in inspected if not item.exists]
    skipped = [item for item in inspected if item.exists]

    if args.dry_run:
        log("==> Dry run")
        log(f"    Ready:   {len(to_publish)}")
        log(f"    Skipped: {len(skipped)}")
        log()
        return 0

    if not to_publish:
        log("==> Nothing to publish")
        return 0

    log("==> Publishing")
    log(f"    Ready:   {len(to_publish)}")
    log(f"    Skipped: {len(skipped)}")
    log()

    err = publish_packages(
        to_publish,
        registry,
        args.tag,
        len(skipped),
    )
    if is_error(err):
        return 1

    return 0


def publish_packages(
    packages: list[Inspection],
    registry: str,
    tag: str | None,
    skipped_count: int,
) -> None | Error:
    success_count = 0
    fail_count = 0
    failed: list[str] = []

    total = len(packages)

    for current, info in enumerate(packages, 1):
        pkg = info.package
        publish_tag = "latest" if info.is_latest else tag or "archive"

        err = npm_publish(pkg.path, registry, publish_tag)
        if is_error(err):
            status = f"FAIL: {err}"
            fail_count += 1
            failed.append(f"{pkg.name}@{pkg.version} ({pkg.path.name})")
        else:
            status = "OK"
            success_count += 1

        print_package_status(current, total, info, status)

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
        return Error("not all packages where published")

    return None


def inspect_package(package: Path, registry: str) -> Inspection | Error:
    info = read_package_info(package)
    if is_error(info):
        return info

    res = npm_version_exists(info.name, info.version, registry)
    if is_error(res):
        return res

    exists, is_latest = res
    return Inspection(
        package=info,
        exists=exists,
        is_latest=is_latest,
    )


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Publish an npmbundle package set to an npm registry",
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
        "--dry-run",
        action="store_true",
        help="inspect packages without publishing",
    )

    if len(sys.argv) == 1:
        parser.print_help()
        sys.exit(0)

    return parser.parse_args()


def print_package_status(
    current: int,
    total: int,
    info: Inspection,
    status: str,
) -> None:
    pkg = info.package
    identity = f"{pkg.name}@{pkg.version}"
    width = len(str(total))
    log(f"[{current:>{width}}/{total}] " f"{identity:<52}  {status}")


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
    if is_error(npm_version):
        return npm_version

    try:
        npm_major, npm_minor = map(int, npm_version.split(".")[:2])
    except ValueError:
        return Error(f"invalid npm version returned: {npm_version}")

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


def read_package_info(path: Path) -> PackageInfo | Error:
    try:
        with tarfile.open(path, mode="r:gz") as archive:
            member = find_package_json(archive)
            if is_error(member):
                return member

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

    return PackageInfo(path=path, name=name, version=version)


def find_package_json(archive: tarfile.TarFile) -> tarfile.TarInfo | Error:
    candidates: list[tarfile.TarInfo] = []

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
        return Error("expected exactly one top-level package.json")

    return candidates[0]


def npm_version_exists(
    name: str, version: str, registry: str
) -> tuple[bool, bool] | Error:
    res = run_command(
        [
            "npm",
            "view",
            name,
            "versions",
            "--json",
            "--fetch-retries=0",
            f"--registry={registry}",
        ],
        capture_output=True,
    )
    if is_error(res):
        return res

    if res.returncode != 0:
        if "E404" in res.stderr:
            versions = []
        else:
            msg = res.stderr.strip()
            if not msg:
                msg = f"npm view failed with exit code {res.returncode}"
            return Error(msg)
    else:
        try:
            versions = json.loads(res.stdout)
        except json.JSONDecodeError as exc:
            return Error(f"invalid npm view response: {exc}")

        if not isinstance(versions, list):
            return Error("npm view returned an unexpected response")

        if not all(isinstance(item, str) for item in versions):
            return Error("npm view response has invalid version entries")

    exists = version in versions

    parsed_versions: list[tuple[str, SemVer]] = []
    for value in versions:
        parsed = SemVer.parse(value)
        if is_error(parsed):
            return Error(
                f"registry returned invalid version '{value}': " f"{parsed.error}"
            )
        parsed_versions.append((value, parsed))

    bundled = SemVer.parse(version)
    if is_error(bundled):
        return Error(f"invalid package version '{version}': " f"{bundled.error}")

    parsed_versions.append((version, bundled))

    stable_versions = [
        (value, parsed) for value, parsed in parsed_versions if not parsed.prerelease
    ]

    if not stable_versions:
        return exists, False

    latest = max(
        stable_versions,
        key=lambda item: (
            item[1].major,
            item[1].minor,
            item[1].patch,
        ),
    )[0]

    return exists, version == latest


def npm_publish(package: Path, registry: str, tag: str) -> None | Error:
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

    res = run_command(command)
    if is_error(res):
        return res

    if res.returncode != 0:
        msg = res.stderr.strip()
        if not msg:
            msg = f"npm publish failed with exit code {res.returncode}"
        return Error(msg)

    return None


def get_registry(registry: str | None) -> str | Error:
    if registry:
        return registry

    res = run_command(
        ["npm", "config", "get", "registry"],
        capture_output=True,
    )
    if is_error(res):
        return res

    if res.returncode != 0:
        msg = res.stderr.strip()
        if not msg:
            msg = "unable to determine npm registry"
        return Error(msg)

    registry = res.stdout.strip()
    if not registry:
        return Error("npm returned an empty registry")

    return res.stdout.strip()


def get_npm_version() -> str | Error:
    res = run_command(
        ["npm", "--version"],
        capture_output=True,
    )
    if is_error(res):
        return res

    if res.returncode != 0:
        msg = res.stderr.strip()
        if not msg:
            msg = "unable to determine npm version"
        return Error(msg)

    version = res.stdout.strip()
    if not version:
        return Error("npm returned no version")

    return version


def run_command(
    args: list[str],
    *,
    capture_output: bool = False,
) -> subprocess.CompletedProcess[str] | Error:
    try:
        return subprocess.run(
            args,
            check=False,
            text=True,
            capture_output=capture_output,
        )
    except OSError as exc:
        return Error(f"running command {exc}")


def command_exists(name: str) -> bool:
    return shutil.which(name) is not None


def log(message: str = "", end="\n") -> None:
    print(message, end=end, file=sys.stderr)


def error(message: str = "") -> None:
    print(f"Error: {message}", file=sys.stderr)


def is_error(value: object) -> TypeIs[Error]:
    return isinstance(value, Error)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        log("\nInterrupted")
        sys.exit(130)
