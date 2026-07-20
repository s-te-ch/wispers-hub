# -*- mode: bazel-build -*-
# SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
# SPDX-License-Identifier: AGPL-3.0-only

# Overlay BUILD file for @connect_client (connect/client).

load("@rules_go//go:def.bzl", "go_library")
load("@rules_proto_grpc_go//:defs.bzl", "go_grpc_library", "go_proto_library")

package(default_visibility = ["//visibility:public"])

sh_binary(
    name = "cargo_build",
    srcs = ["cargo_build.sh"],
)

genrule(
    name = "wispers_connect_cdylib_darwin",
    srcs = glob(["wispers-connect/src/**", "wispers-connect/Cargo.toml"]),
    outs = ["libwispers_connect.dylib"],
    cmd = "$(location :cargo_build) $@",
    local = True,
    tags = ["no-sandbox"],
    tools = [":cargo_build"],
)

genrule(
    name = "wispers_connect_cdylib_linux",
    srcs = glob(["wispers-connect/src/**", "wispers-connect/Cargo.toml"]),
    outs = ["libwispers_connect.so"],
    cmd = "$(location :cargo_build) $@",
    local = True,
    tags = ["no-sandbox"],
    tools = [":cargo_build"],
)

cc_import(
    name = "wispers_connect_cdylib",
    shared_library = select({
        "@bazel_tools//src/conditions:darwin": ":wispers_connect_cdylib_darwin",
        "//conditions:default": ":wispers_connect_cdylib_linux",
    }),
)

cc_library(
    name = "wispers_connect_lib",
    hdrs = ["wispers-connect/include/wispers_connect.h"],
    includes = ["wispers-connect/include"],
    deps = [":wispers_connect_cdylib"],
)

# Go wrapper for wispers-connect.
go_library(
    name = "wispersgo",
    srcs = glob(
        ["wrappers/go/*.go", "wrappers/go/*.h"],
        exclude = ["wrappers/go/*_test.go"],
    ),
    importpath = "wispersgo",
    cgo = True,
    cdeps = [":wispers_connect_lib"],
)

proto_library(
    name = "hub_proto",
    srcs = ["wispers-connect/proto/hub.proto"],
    deps = [":roster_proto"],
)

go_grpc_library(
    name = "hub_go_grpc",
    importpath = "connect/client/proto/hubpb",
    protos = [":hub_proto"],
    deps = [":roster_go_proto"],
)

proto_library(
    name = "roster_proto",
    srcs = ["wispers-connect/proto/roster.proto"],
    strip_import_prefix = "/wispers-connect/proto",
)

go_proto_library(
    name = "roster_go_proto",
    importpath = "connect/client/proto/rosterpb",
    protos = [":roster_proto"],
)
