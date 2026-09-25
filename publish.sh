#!/usr/bin/env bash

for pkg in ./npm-bundle/*.tgz; do
  npm publish "$pkg" --registry http://127.0.0.1:4873 --access public
done
