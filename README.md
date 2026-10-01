# npmbundle

`npmbundle` packages npm package(s) and its complete dependency tree into a single, self-contained archive.

The resulting bundle contains all required `.tgz` package artifacts and an executable `publish.sh` script that publishes them to an npm-compatible registry.

Designed for environments where packages need to be transferred and published without relying on npm having to resolve and download dependencies again.

## Features

* Bundles an npm package and its dependencies
* Produces a single `.tgz` archive
* Includes all package `.tgz` files in the bundle
* Includes an executable `publish.sh` publishing script
* Works with npm-compatible registries
* Supports resuming interrupted builds
* Linux binaries for `amd64` and `arm64`
* No persistent cache is maintained

## Installation

Download the appropriate binary from the GitHub Releases page and place `npmbundle` somewhere in your `PATH`.

Verify the installation:

```bash
npmbundle --help
```

## Usage

Bundle a npm package(s) or a file:

```bash
npmbundle axios@latest axios@1.0.0
npmbundle npm-packages.txt
```

The output archive looks like:

```text
npmbundle.tgz
└── npmbundle-output/
    ├── package-a-1.2.3.tgz
    ├── package-a-3.2.2.tgz
    ├── my-package-1.0.0.tgz
    └── publish.sh
```

Extract the bundle:

```bash
tar -xzf npmbundle.tgz
cd npmbundle-output
```

Then publish the packages:

```bash
./npmbundle-output/publish.sh
```

## Resuming interrupted builds

`npmbundle` uses a deterministic temporary workspace under `/tmp`:

```text
/tmp/npmbundle-<digest>/
```

If a build fails, the workspace is left in place. Running the same command again can reuse package artifacts that were already downloaded instead of downloading them again.

Successful builds clean up the temporary workspace automatically.
