#!/usr/bin/env bash

for tarball in result/tarballs/*.tgz; do
  echo "Publishing $tarball..."
  npm publish "$tarball" || echo "Skipped (already exists)"
done
