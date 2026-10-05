#!/usr/bin/env bash

set -uo pipefail

# 1. Show usage if requested or if no arguments are given
if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]] || [ -z "${1:-}" ]; then
    echo "Usage: $(basename "$0") <PACKAGE_DIR> [REGISTRY]"
    echo ""
    echo "Arguments:"
    echo "  PACKAGE_DIR    Directory containing .tgz or .tar.gz package bundles"
    echo "  REGISTRY       Target npm registry URL (optional)"
    echo ""
    echo "Example:"
    echo "  $(basename "$0") ./my-bundles http://localhost:4873"
    exit 0
fi

PACKAGE_DIR="${1%/}"
REGISTRY="${2:-$(npm config get registry)}"

if [ "$PACKAGE_DIR" = "." ]; then
    search_dir="."
else
    search_dir="./${PACKAGE_DIR#./}"
fi

if [ ! -d "$PACKAGE_DIR" ]; then
    echo "Error: Directory '$PACKAGE_DIR' not found."
    exit 1
fi

# Find all tarballs safely using nullglob
shopt -s nullglob
tarballs=("$search_dir"/*.tgz "$search_dir"/*.tar.gz)
shopt -u nullglob

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

    # 2. Handle private/scoped packages (__ becomes / with a leading @)
    if [[ "$base" == *__* ]]; then
        base="@${base//__//}"
    fi

    # 3. Extract name and version
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

    # Determine tags to avoid 'latest' collisions during bulk historical imports
    publish_args=("--registry=$REGISTRY" "--ignore-scripts" "--access=public" "--provenance=false" "--silent")
    if [[ "$pkg_version" == *-* ]]; then
        publish_args+=("--tag" "next")
    else
        publish_args+=("--tag" "archive")
    fi

    # Publish to registry
    if npm publish "$tarball" "${publish_args[@]}"; then
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
