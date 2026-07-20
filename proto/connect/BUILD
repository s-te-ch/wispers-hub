# SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
# SPDX-License-Identifier: AGPL-3.0-only

load("@rules_proto_grpc_go//:defs.bzl", "go_grpc_library")

package(default_visibility = ["//visibility:public"])

proto_library(
    name = "be_proto",
    srcs = ["be.proto"],
)

go_grpc_library(
    name = "be_go_grpc",
    importpath = "connect/proto/bepb",
    protos = [":be_proto"],
)
