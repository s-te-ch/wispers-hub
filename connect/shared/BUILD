# SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
# SPDX-License-Identifier: AGPL-3.0-only

# Code shared between Connect components (be, hub).
# This sits on the open-source side of the line.

load("@rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "attestation",
    srcs = ["attestation.go"],
    importpath = "connect/shared/attestation",
    visibility = ["//connect:__subpackages__"],
    deps = [
        "@com_github_golang_jwt_jwt_v5//:jwt",
    ],
)

go_library(
    name = "apikeys",
    srcs = ["apikeys.go"],
    importpath = "connect/shared/apikeys",
    visibility = ["//connect:__subpackages__"],
)

go_library(
    name = "integratorapi",
    srcs = ["integratorapi.go"],
    importpath = "connect/shared/integratorapi",
    visibility = ["//connect:__subpackages__"],
)

go_test(
    name = "apikeys_test",
    srcs = [
        "apikeys.go",
        "apikeys_test.go",
    ],
)
