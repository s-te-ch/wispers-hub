# Example of self-hosting the Wispers Connect hub

This example sets up a standalone, functional backend that's usable with the
Wispers Connect client library. It runs three containers: the hub, in standalone
mode with state stored in sqlite; a coturn STUN server for address discovery
during connection establishment, and a TLS front (caddy) with automatic Let's
Encrypt certificates.

## Steps

Point a public DNS name at this machine first. Certificate issuance needs it to
resolve. Then:

```sh
export PUBLIC_HOST=connect.example.com   # your DNS name
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
  3478/udp+tcp (STUN).
- **Certificates** are provisioned and renewed automatically by caddy (stored
  in the `caddy-data` volume). If certificate issuance fails, check that
  $PUBLIC_HOST resolves to this machine and port 80 is reachable.
