# SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
# SPDX-License-Identifier: AGPL-3.0-only

# Wispers Connect hub

load("@rules_go//go:def.bzl", "go_binary", "go_library", "go_test")
load("@rules_pkg//pkg:tar.bzl", "pkg_tar")
load("@rules_oci//oci:defs.bzl", "oci_image", "oci_image_index", "oci_load", "oci_push")

go_binary(
    name = "hub",
    srcs = [
        "hub.go",
        "hub_flagdefs.go",
    ],
    deps = [
        ":hubsrv",
        ":standalone",
        "//proto/connect:be_go_grpc",
        "@connect_client//:hub_go_grpc",
        "@org_golang_google_grpc//:grpc",
        "@org_golang_google_grpc//credentials/insecure",
        "@org_golang_google_grpc//reflection",
    ],
)

go_test(
    name = "hub_test",
    srcs = [
        "hub.go",
        "hub_flagdefs.go",
        "hub_test.go",
    ],
    deps = [
        ":hubsrv",
        ":standalone",
        "//proto/connect:be_go_grpc",
        "@connect_client//:hub_go_grpc",
        "@org_golang_google_grpc//:grpc",
        "@org_golang_google_grpc//codes",
        "@org_golang_google_grpc//credentials/insecure",
        "@org_golang_google_grpc//metadata",
        "@org_golang_google_grpc//reflection",
        "@org_golang_google_grpc//status",
        "@org_golang_google_grpc//test/bufconn",
    ],
)

go_test(
    name = "turn_test",
    srcs = ["turn_test.go"],
    embed = [":hubsrv"],
    deps = [
        "//connect/shared:turncreds",
        "//proto/connect:be_go_grpc",
        "@connect_client//:hub_go_grpc",
        "@org_golang_google_grpc//:grpc",
        "@org_golang_google_grpc//metadata",
    ],
)

go_test(
    name = "version_test",
    srcs = ["version_test.go"],
    embed = [":hubsrv"],
    deps = [
        "@org_golang_google_grpc//codes",
        "@org_golang_google_grpc//metadata",
        "@org_golang_google_grpc//status",
    ],
)

go_library(
    name = "standalone",
    srcs = [
        "standalone.go",
        "standalone_grpc.go",
        "standalone_rest.go",
    ],
    embedsrcs = ["standalone_schema.sql"],
    importpath = "connect/hub/standalone",
    deps = [
        "//connect/shared:apikeys",
        "//connect/shared:attestation",
        "//connect/shared:integratorapi",
        "//proto/connect:be_go_grpc",
        "@com_github_gin_gonic_gin//:gin",
        "@com_github_google_uuid//:uuid",
        "@org_golang_google_grpc//:grpc",
        "@org_golang_google_grpc//codes",
        "@org_golang_google_grpc//credentials/insecure",
        "@org_golang_google_grpc//status",
        "@org_golang_google_grpc//test/bufconn",
        "@org_golang_google_protobuf//proto",
        "@org_golang_x_time//rate",
        "@org_modernc_sqlite//:sqlite",
        "@connect_client//:roster_go_proto",
    ],
)

go_test(
    name = "standalone_test",
    srcs = ["standalone_test.go"],
    deps = [
        ":standalone",
        "//proto/connect:be_go_grpc",
        "@connect_client//:roster_go_proto",
        "@org_golang_google_grpc//codes",
        "@org_golang_google_grpc//status",
        "@org_golang_google_protobuf//proto",
    ],
)

go_library(
    name = "hubsrv",
    srcs = [
        "hubsrv.go",
        "version.go",
    ],
    importpath = "connect/hub/hubsrv",
    deps = [
        ":routing",
        "//connect/shared:turncreds",
        "//proto/connect:be_go_grpc",
        "@connect_client//:hub_go_grpc",
        "@connect_client//:roster_go_proto",
        "@org_golang_google_grpc//:grpc",
        "@org_golang_google_grpc//codes",
        "@org_golang_google_grpc//metadata",
        "@org_golang_google_grpc//status",
        "@org_golang_google_protobuf//proto",
        "@org_golang_x_time//rate",
    ],
)

go_library(
    name = "routing",
    srcs = ["routing.go"],
    importpath = "connect/hub/routing",
    deps = [
        "@connect_client//:hub_go_grpc",
        "@org_golang_google_grpc//codes",
        "@org_golang_google_grpc//status",
    ],
)

## Docker image ################################################################

pkg_tar(
    name = "tar",
    srcs = [":hub"],
)

oci_image(
    name = "image",
    base = "@distroless_static",
    tars = [":tar"],
    entrypoint = ["/hub"],
)

oci_image_index(
    name = "image_multiarch",
    images = [":image"],
    platforms = [
        "@rules_go//go/toolchain:linux_amd64",
        "@rules_go//go/toolchain:linux_arm64",
    ],
)

oci_load(
    name = "load_dev_image",
    image = ":image",
    repo_tags = ["wispers/connect/hub:local"],
)

oci_push(
    name = "push_to_staging",
    image = ":image_multiarch",
    repository = "ghcr.io/s-te-ch/wispers/connect/hub",
    remote_tags = ["staging"],
)

# The public self-hosting image: :image plus the OCI labels of the published
# artifact. GHCR links the package to the source repo through the source
# label; the licenses label states the outbound license, which is why the
# internal images above don't carry it.
oci_image(
    name = "image_public",
    base = ":image",
    labels = {
        "org.opencontainers.image.source": "https://github.com/s-te-ch/wispers-hub",
        "org.opencontainers.image.licenses": "AGPL-3.0-only",
    },
)

oci_image_index(
    name = "image_public_multiarch",
    images = [":image_public"],
    platforms = [
        "@rules_go//go/toolchain:linux_amd64",
        "@rules_go//go/toolchain:linux_arm64",
    ],
)

# Deliberately has no remote_tags. `tools/export/export_hub.sh release`
# supplies :latest and :vX.Y.Z, building from the export tree so the public
# image always corresponds to public source.
oci_push(
    name = "push_public",
    image = ":image_public_multiarch",
    repository = "ghcr.io/s-te-ch/wispers-hub",
)
