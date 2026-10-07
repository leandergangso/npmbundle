# npmbundle

This archive contains npm package tarballs for import into an npm-compatible registry.

## Contents

```text
*.tgz           npm package tarballs
publish.py      package publisher
README.md       this file
```

## Publish

Inspect the packages without making changes:

```bash
./publish.py .
```

Publish them:

```bash
./publish.py . --execute
```

Specify a registry explicitly if required (defaults to npm config):

```bash
./publish.py . https://registry.example.com/npm/ --execute
```

Packages are published with the `archive` dist-tag by default. Use `--tag` to specify another tag.  
The `latest` tag are handled automatically for you.

## Requirements

- Python 3.9+
- npm CLI
- Credentials with permission to publish to the target registry
