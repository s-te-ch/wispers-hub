# Contributing

Thanks for your interest in contributing!

## Contributions are paused for now

We are still finalizing our Contributor License Agreement, and until that is
done we unfortunately cannot accept code contributions. This section will be
replaced with the real process once the CLA is ready.

**Issues and bug reports are very welcome** in the meantime.

## How this repo works

This repo is a read-only export of the Wispers monorepo. Once contributions
open up, accepted PRs are imported into the monorepo and land back in this
repo through the export (you keep authorship).

## Building and testing

```
bazel test //...
```

builds everything and runs the tests (install
[bazelisk](https://github.com/bazelbuild/bazelisk); it picks up
`.bazelversion`). See the [README](README.md) for what lives where.
