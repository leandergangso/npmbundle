#!/usr/bin/env python3

import argparse
import json
import shutil
import subprocess
import sys
import tarfile
from pathlib import Path
from typing import Optional
from time import sleep

MIN_PYTHON = (3, 9)


def main() -> int:
    python_version = check_python()

    args = parse_args()

    if not command_exists("npm"):
        print("Error: npm command not found.", file=sys.stderr)
        return 1

    registry = args.registry or get_default_registry()

    try:
        packages = find_packages(args.package_dir)
    except ValueError as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 1

    print()
    print("npmbundle publisher")
    print()
    print("Environment:")
    print(f"  Python: {python_version:<12} OK")
    print(f"  npm:    {get_npm_version():<12} OK")
    print()
    print(f"==> Found {len(packages)} package(s)")
    print(f"==> Registry: {registry}")
    print()
    if not args.execute:
        print("==> Dry run: no packages will be published.")
        print("    Use --execute to publish.")
        print()
        sleep(1)

    inspected = []
    total = len(packages)

    for current, package in enumerate(packages, 1):
        name, version, status = inspect_package(package, registry)
        inspected.append((package, name, version, status))
        if name and version:
            print_package_status(current, total, package, name, version, status)
    print()

    invalid = [item for item in inspected if item[3].startswith("INVALID")]
    if invalid:
        print(f"==> Inspection failed: {len(invalid)} " f"invalid package(s).")
        return 1

    to_publish = [item for item in inspected if item[3] == "PUBLISH"]
    skipped = [item for item in inspected if item[3] == "SKIPPED"]

    if not args.execute:
        print("==> Dry run: no packages were published.")
        print(f"    Ready:   {len(to_publish)}")
        print(f"    Skipped: {len(skipped)}")
        print()
        print("Use --execute to publish.")
        return 0

    if not to_publish:
        print("==> Nothing to publish.")
        return 0

    return publish_packages(
        to_publish,
        registry,
        args.tag,
        len(skipped),
    )


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

    print(f"[{current:>{width}}/{total}] " f"{identity:<46}  {status}")


def publish_packages(
    packages: list[tuple[Path, str, str, str]],
    registry: str,
    tag: Optional[str],
    skipped_count: int,
) -> int:
    success_count = 0
    fail_count = 0
    failed = []

    print("==> Publishing")
    print()

    for package, name, version, _ in packages:
        publish_tag: str
        if tag:
            publish_tag = tag
        else:
            publish_tag = "archive"

        print(f"[PUBLISH] {name}@{version} --tag {publish_tag}")

        if npm_publish(package, registry, publish_tag):
            print(f"[OK]      {name}@{version}")
            success_count += 1
        else:
            print(f"[FAIL]    {name}@{version}")
            fail_count += 1
            failed.append(f"{name}@{version} ({package.name})")

    print()
    print("==> Publish Summary:")
    print(f"    Success: {success_count}")
    print(f"    Skipped: {skipped_count} (already existed)")
    print(f"    Failed:  {fail_count}")

    if failed:
        print()
        print("==> Failed Packages:")
        for package in failed:
            print(f"    - {package}")
        return 1

    return 0


def inspect_package(package: Path, registry: str) -> tuple[str, str, str]:
    try:
        package_name, package_version = read_package_metadata(package)
    except ValueError as exc:
        return "", "", f"INVALID: {exc}"

    existing = npm_view(package_name, package_version, registry)
    if existing is not None:
        return package_name, package_version, " SKIPPED"

    return (
        package_name,
        package_version,
        f"+PUBLISH",
    )


def check_python() -> str:
    if sys.version_info < MIN_PYTHON:
        sys.exit(
            f"Python {MIN_PYTHON[0]}.{MIN_PYTHON[1]} or newer is required "
            f"(found {sys.version_info.major}.{sys.version_info.minor})."
        )
    return (
        f"{sys.version_info.major}."
        f"{sys.version_info.minor}."
        f"{sys.version_info.micro}"
    )


def command_exists(name: str) -> bool:
    return shutil.which(name) is not None


def find_packages(directory: Path) -> list[Path]:
    if not directory.exists():
        raise ValueError(f"directory '{directory}' not found")

    if not directory.is_dir():
        raise ValueError(f"'{directory}' is not a directory")

    packages = sorted(
        [
            *directory.glob("*.tgz"),
            *directory.glob("*.tar.gz"),
        ]
    )

    if not packages:
        raise ValueError(f"no package archives found in '{directory}'")

    return packages


def read_package_metadata(path: Path) -> tuple[str, str]:
    try:
        with tarfile.open(path, mode="r:gz") as archive:
            member = find_package_json(archive)

            if not member.isfile():
                raise ValueError("package.json is not a regular file")

            file = archive.extractfile(member)

            if file is None:
                raise ValueError("unable to read package.json")

            metadata = json.load(file)

    except tarfile.TarError as exc:
        raise ValueError(f"invalid tar archive: {exc}") from exc
    except json.JSONDecodeError as exc:
        raise ValueError(f"invalid package.json: {exc}") from exc

    name = metadata.get("name")
    version = metadata.get("version")

    if not isinstance(name, str) or not name:
        raise ValueError("package.json has no valid 'name'")

    if not isinstance(version, str) or not version:
        raise ValueError("package.json has no valid 'version'")

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


def npm_view(name: str, version: str, registry: str) -> Optional[str]:
    result = run_command(
        [
            "npm",
            "view",
            f"{name}@{version}",
            "version",
            "--fetch-retries=0",
            f"--registry={registry}",
        ],
        capture_output=True,
    )

    if result.returncode != 0:
        return None

    version_output = result.stdout.strip()

    if not version_output:
        return None

    return version_output


def npm_publish(package: Path, registry: str, tag: str) -> bool:
    command = [
        "npm",
        "publish",
        str(package),
        f"--registry={registry}",
        "--fetch-retires=0",
        "--ignore-scripts",
        "--access=public",
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
        return "unknown"

    return result.stdout.strip()


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
        print(f"Error running command: {exc}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print("\nInterrupted.", file=sys.stderr)
        sys.exit(130)
