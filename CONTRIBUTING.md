# Contributing

Thanks for considering a contribution!

## How this repo works

This repo is a read-only export of the Wispers monorepo. Issues and pull
requests are very welcome here. Accepted PRs are imported into the monorepo
and land back in this repo through the export (you keep authorship).

## License and CLA

The code is licensed under the [AGPL-3.0](LICENSE). Contributions require
signing the [Contributor License Agreement](CLA.md). The CLA bot will prompt
you on your first pull request, and signing is a one-time PR comment.
The short reason for the CLA: this code also powers the hosted Wispers
Connect service, and the CLA lets your contribution flow into both; your code
always remains available under the AGPL.

## Building and testing

```
bazel test //...
```

builds everything and runs the tests (install
[bazelisk](https://github.com/bazelbuild/bazelisk); it picks up
`.bazelversion`). See the [README](README.md) for what lives where.
