#!/usr/bin/env bash

set -uo pipefail

PACKAGE_DIR="${1:-.}"
PACKAGE_DIR="${PACKAGE_DIR%/}"
if [ "$PACKAGE_DIR" = "." ]; then
    search_dir="."
else
    search_dir="./${PACKAGE_DIR#./}"
fi

REGISTRY="${2:-$(npm config get registry)}"

if [ ! -d "$PACKAGE_DIR" ]; then
    echo "Error: Directory '$PACKAGE_DIR' not found."
    exit 1
fi

# Find all tarballs (.tgz and .tar.gz) using nullglob for safety
shopt -s nullglob
tarballs=("$search_dir"/*.tgz "$search_dir"/*.tar.gz)
total=${#tarballs[@]}

if [ "$total" -eq 0 ]; then
    echo "No package archives found in '$PACKAGE_DIR'."
    exit 0
fi

echo "==> Publishing $total packages to registry: $REGISTRY"
echo ""
echo "starting in 3 seconds..."
sleep 3
echo ""

success_count=0
skip_count=0
fail_count=0
current=0
width=${#total}
failed_list=()

for tarball in "${tarballs[@]}"; do
    current=$((current + 1))
    printf -v count_str "[%*d/%d]" "$width" "$current" "$total"

    filename=$(basename "$tarball")

    # 1. Strip file extension (.tgz or .tar.gz)
    base="${filename%.tar.gz}"
    base="${base%.tgz}"

    # 2. Handle private/scoped packages (all __ becomes / with a leading @)
    if [[ "$base" == *__* ]]; then
        base="@${base//__//}"
    fi

    # 3. Extract name and version (splits at the last hyphen before the version number)
    # Using built-in Bash regex so it safely handles prerelease tags (e.g. 1.0.0-rc.1)
    if [[ $base =~ ^(.*)-([0-9]+\..*)$ ]]; then
        pkg_name="${BASH_REMATCH[1]}"
        pkg_version="${BASH_REMATCH[2]}"
        pkg_spec="${pkg_name}@${pkg_version}"
    else
        echo "$count_str Failed to parse package info from filename: $filename"
        fail_count=$((fail_count + 1))
        failed_list+=("$filename (parse error)")
        continue
    fi

    # Idempotency check: Skip if version already exists on target registry
    existing=$(npm view "$pkg_spec" version --registry="$REGISTRY" 2>/dev/null || true)
    if [ -n "$existing" ]; then
        echo "$count_str $pkg_spec | SKIPPED"
        skip_count=$((skip_count + 1))
        continue
    fi

    # Determine if it's a prerelease version (contains a hyphen after version numbers, e.g., 1.0.0-rc.1)
    publish_args=("--registry=$REGISTRY" "--ignore-scripts" "--access=public" "--provenance=false" "--silent")
    if [[ "$pkg_version" == *-* ]]; then
        # npm requires a custom dist-tag for prereleases so they don't overwrite 'latest'
        publish_args+=("--tag" "next")
    fi

    # Publish to registry
    if npm publish "$tarball" "${publish_args[@]}" &>/dev/null; then
        echo "$count_str $pkg_spec"
        success_count=$((success_count + 1))
    else
        echo "$count_str $pkg_spec | ERROR: Publish failed"
        fail_count=$((fail_count + 1))
        failed_list+=("$pkg_spec ($filename)")
    fi
done

echo ""
echo "==> Publish Summary:"
echo "    Success: $success_count"
echo "    Skipped: $skip_count (already existed)"
echo "    Failed:  $fail_count"

if [ "$fail_count" -gt 0 ]; then
    echo ""
    echo "==> Failed Packages:"
    for failed in "${failed_list[@]}"; do
        echo "    - $failed"
    done
    exit 1
fi
