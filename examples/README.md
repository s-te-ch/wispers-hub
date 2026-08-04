# Example of self-hosting the Wispers Connect hub

This example sets up a standalone, functional backend that's usable with the
Wispers Connect client library. It runs three containers: the hub, in standalone
mode with state stored in sqlite; a coturn server (for address discovery during
connection establishment and, if enabled, traffic relaying for node pairs where
NAT traversing connections can't be established), and a TLS front (caddy) with
automatic Let's Encrypt certificates.

## Minimal setup

Point a public DNS name at this machine first. Certificate issuance needs it to
resolve. Then:

```sh
echo "PUBLIC_HOST=connect.example.com" > .env       # your DNS name
docker compose up -d
cat data/initial-api-key
```

On first start the hub mints your admin API key and writes it to
`data/initial-api-key` (it's also in `docker compose logs hub`). Store it in
your password manager, then delete the file. You're done.

Everything else happens over the REST and gRPC APIs . You can point the tools
from the [Wispers Connect library](https://github.com/s-te-ch/wispers-client),
by pointing them at your backend (`wcadm --url=https://connect.example.com` and
`wconnect --hub=https://connect.example.com`, respectively), or you can use the
API by hand like:

```sh
curl -H "Authorization: Bearer $KEY" https://$PUBLIC_HOST/api/v1/stats
```

## Operational notes

- **State** is a single sqlite file at `./data/hub.db`, bind-mounted into the
  container. Live-safe backup:
  `sqlite3 data/hub.db ".backup data/hub-backup.db"`.
- **Lost API key?**: stop the hub (`docker compose stop hub`), delete all API
  keys (`sqlite3 data/hub.db "DELETE FROM api_keys"`), start it again. A hub
  with no active keys mints a fresh one, writing it to `data/initial-api-key`
  again.
- **Firewall**: open 80/tcp and 443/tcp (caddy: ACME + all hub traffic) and
  3478/udp+tcp (STUN). With TURN enabled, additionally 49152-49407/udp (see
  "Enabling TURN" below).
- **Certificates** are provisioned and renewed automatically by caddy (stored
  in the `caddy-data` volume). If certificate issuance fails, check that
  $PUBLIC_HOST resolves to this machine and port 80 is reachable.

## Enabling TURN (recommended)

The default setup uses only STUN, limiting clients to direct peer-to-peer
connections. However, there are some configurations where this doesn't work
(e.g. two symmetric NATs). In those cases, TURN relaying helps. It's off by
default to make setup minimal. To activate TURN relaying:

1. Add two lines to `.env` and restart:

   ```sh
   echo "COMPOSE_FILE=docker-compose.yml:docker-compose.turn.yml" >> .env
   echo "TURN_SECRET=$(openssl rand -base64 32)" >> .env
   docker compose up -d
   ```

2. Open **49152-49407/udp** in the firewall (relay allocations).

**Verify it actually works**. A broken relay fails silently until the first
NAT-hostile pair needs it, so it's better to check now. From a machine on a
*different* network:

```sh
docker run --rm coturn/coturn:4 turnutils_uclient -y \
	-W "$TURN_SECRET" <your PUBLIC_HOST>
```

A working relay reports round-trips ("total send dropped 0"). Timeouts mean
the relay ports aren't reachable — check the firewall.

Notes:

- The secret is shared between the hub (mints short-lived relay credentials for
  nodes) and coturn (validates them); it lives only in `.env`, which must
  survive restarts. Rotating it is safe as long as hub and coturn restart
  together (`docker compose up -d` after editing `.env`).
- `turnserver-turn.conf` denies relaying into private address ranges. The
  denylist should be good enough for most deployments, but you may want to check
  that it fits yours.
- There is no bandwidth limit configured and all nodes share the relay
  unmetered. coturn's `max-bps` (per session) is available if you need a crude
  cap.
