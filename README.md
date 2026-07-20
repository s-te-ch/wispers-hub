# Wispers Connect Hub

The rendezvous and signaling server of Wispers Connect. You can find the client
library [here](https://github.com/s-te-ch/wispers-client)) or read more about
Wispers at https://wispers.dev.

This repo contains:

- `connect/hub` — the hub server.
- `connect/shared` — protocol pieces shared with the hosted backend.
- `proto/connect` — the gRPC interface between the hub and its storage
  backend.

## Managed vs Standalone modes

This server is used by both managed and self-hosted backends of Wispers Connect.

In **managed** mode, the hub serves only the gRPC API used by Wispers Connect
nodes, as one of a number of shards. The shards all used the same storage
backend through gRPC. Connectivity group management (e.g. using `wcadm`) goes
through another server's REST API and is available at
https://connect.wispers.dev/api.

In **standalone** mode (the default) the hub runs single-sharded and implements
both storage and connectivity group management internally, using a sqlite
database. This is more adequate for self-hosting setups.

## Quickstart

### Managed

The easy way to use Wispers Connect is to just use the managed version. Sign up
at https://connect.wispers.dev.

### Self-hosting

If you'd rather self-host the backend, the files in [examples](examples/) are
for you. You'll find a docker-compose setup (hub + STUN server) that gets you a
functional, self-hosted Wispers Connect backend.

The short version:

1. Start the hub in standalone mode with `--stun-server` pointing at a STUN
   server you run or trust.
2. On first start, the hub will mint an API key, written to `initial-api-key`
   next to the state DB and printed to stdout to the log. Store it!
3. Point the Wispers Connect tools (found in the
   [client repo](https://github.com/s-te-ch/wispers-client)) at your instance:
   - `wcadm --url=<your instance>` to configure your connectivity groups
   - `wconnect --hub=<your instance>` to test the instance.
   Or if you're using the library directly, set the corresponding values in the
   code.

## Building

Builds with [Bazel](https://bazel.build) (via
[bazelisk](https://github.com/bazelbuild/bazelisk)):

```
bazel test //...
bazel run //connect/hub -- --stun-server <host:port>
```

## Development

This repo is a read-only export of the Wispers monorepo; issues and bug
reports are welcome here. Code contributions are not accepted yet — see
[CONTRIBUTING.md](CONTRIBUTING.md).

## Version compatibility

Hub and client library are versioned independently and don't need to be upgraded
in lockstep. The mutually exchange version info when communicating and will
enforce a compatibility window. Both hub and client library releases state the
minimum compatible version, if applicable.

## License

[AGPL-3.0-only](LICENSE).
