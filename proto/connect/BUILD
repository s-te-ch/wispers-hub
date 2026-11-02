# SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
# SPDX-License-Identifier: AGPL-3.0-only

load("@rules_proto_grpc_go//:defs.bzl", "go_grpc_library", "go_proto_library")

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

# The client library's wire protocol, as Go stubs for the hub. The .proto
# files live in the client module; only the language bindings are ours.
go_grpc_library(
    name = "hub_go_grpc",
    importpath = "connect/client/proto/hubpb",
    protos = ["@wispers_connect//wispers-connect/proto:hub_proto"],
    deps = [":roster_go_proto"],
)

go_proto_library(
    name = "roster_go_proto",
    importpath = "connect/client/proto/rosterpb",
    protos = ["@wispers_connect//wispers-connect/proto:roster_proto"],
)
