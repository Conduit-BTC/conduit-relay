# Congee source and releases

The production Congee executable lives in `congee/`. The root Go module and root Dockerfile build the separate legacy relay.

The first Conduit source release is `congee-v2026.09.29.1`. Keep this version pinned until a reviewed update replaces it. Do not deploy a moving `latest` tag.

## Source provenance

The imported source uses the upstream MIT license. Its module path remains unchanged.

- Baseline: [`michmich112/congee` develop at `c7456123896f12a148e214307b357abb6619f714`](https://github.com/michmich112/congee/tree/c7456123896f12a148e214307b357abb6619f714).
- Recipient protection: [upstream PR 52](https://github.com/michmich112/congee/pull/52), commit `de2b2d275dbf779244a3fc4fc48a271298a95fc5`. The upstream scheduler guard was adapted to this baseline.
- Search index repair: [upstream PR 53](https://github.com/michmich112/congee/pull/53), commit `6dddeca251427d0e5612b01605a19473255c5d4c`.
- Conduit changes: public browser discovery, positive duplicate acknowledgments after validation, and startup validation of the schema-8 search layout.
- Review hardening: reject padded recipient keys and reserve NIP-77 session capacity atomically, including queued loads and replacements.
- Admission hardening: trust forwarded client IPs only from configured proxies, reserve global connection capacity atomically, and close failed writers.
- Storage and payload hardening: record fresh PostgreSQL migrations atomically and bound WebSocket payload expansion during reads.
- Startup and subscription hardening: initialize fresh PostgreSQL databases, preserve timestamp-tied query results, and isolate replacement subscription snapshots.
- Runtime hardening: match subscriptions before visibility checks, release the subscription lock during storage reads, stop canceled snapshots before further visibility reads, and shut down after listener failure.
- Admin dependency lock: compatible dependency updates remove known high-severity build-tool findings.

This baseline includes replaceable revision ordering and NIP-50 ranking. The search index uses schema version 8 with the `event_fts_rowids` mapping. Do not combine this release with another upstream migration that also claims version 8. Startup rejects incompatible schema-8 search tables, indexes, or triggers.

The previous deployed image does not have a recoverable source revision. This import establishes a source baseline; it does not prove equivalence with every previous binary patch.

## Public relay access

The production entrypoint accepts standard Nostr WebSocket clients from external sites, local applications, and clients without an Origin header. Keep the existing write policy, event validation, authentication, and admission limits. Protected events still require the applicable reader authentication.

NIP-11 discovery and its image assets allow browser reads from any origin. The entrypoint keeps this public discovery CORS enabled regardless of the upstream configuration flag. [NIP-11](https://github.com/nostr-protocol/nips/blob/master/11.md) requires browser CORS support for relay information.

## HTTP enhancement boundary

This source does not expose a public HTTP search or enrichment API. Plugins can intercept ordinary WebSocket `REQ` messages. Opening standard WebSockets also leaves that existing plugin path accessible. This release does not implement a separate enhancement entitlement or payment boundary.

The HTTP plugin routes on the admin server remain admin APIs. Keep their Bearer authentication and sandboxed plugin UI behavior. Do not use privileged plugin admin actions as a public search transport.

A future public HTTP enhancement endpoint must apply its access policy before plugin work. Its initial browser policy must allow only these HTTPS origins:

- `https://shop.conduit.market`
- `https://sell.conduit.market`
- `https://<one-dns-label>.conduit-market-coo.pages.dev`
- `https://<one-dns-label>.conduit-merchant-33n.pages.dev`

Keep that endpoint's origin checks and rate limits separate from ordinary relay access. An Origin header identifies browser context; it cannot authenticate a client. A restricted HTTP endpoint also needs to prevent equivalent enhanced work through the existing WebSocket interception path.

## Proxy and connection admission

The global connection cap includes pending upgrades. Failed admission releases its reservation.
Direct connections use the socket peer IP and ignore all client-IP headers.

Before deployment, configure `connection_limits.trusted_proxy_cidrs` with the actual ingress proxy networks.
An empty list trusts no proxy. Do not use public client networks or catch-all CIDRs.
Select `connection_limits.trusted_proxy_client_ip_header` only when that proxy overwrites the header.
For Fly HTTP ingress, select [`Fly-Client-IP`](https://www.fly.io/docs/networking/request-headers/); do not trust client-supplied Cloudflare headers through Fly.
For direct Cloudflare ingress, select `CF-Connecting-IP` and trust only that ingress.
The default `X-Forwarded-For` mode walks the chain from the nearest proxy to the first untrusted address.
Changes to proxy trust require a restart. Rehearse admission with two clients and forged headers before cutover.
Stop if clients share an unexpected limiter bucket or a forged header changes their resolved IP.

## Build and validation

Run these commands from the repository root:

```sh
cd congee
CGO_ENABLED=1 go test -count=1 ./...
CGO_ENABLED=1 go test -race -count=1 ./...
go vet ./...
cd ..
docker build --build-arg VERSION="$(cat congee/VERSION)" \
  --build-arg GIT_REVISION="$(git rev-parse HEAD)" \
  -t conduit-congee:review congee
python3 scripts/congee-smoke.py conduit-congee:review
```

The image includes the static admin UI. CGO is required for libSQL. OCI labels identify this repository, the source commit, and the binary version.

The Congee workflow runs fresh PostgreSQL startup and migration rollback regressions against an ephemeral PostgreSQL 17 service, with normal and race checks. These tests create and remove isolated schemas. Other PostgreSQL integration tests remain opt-in through `TEST_POSTGRES_DSN`; use a disposable test database because existing fixtures share rows and schema state. CI does not set this variable for the full suite.

## Release handoff

1. Merge the reviewed PR into `main` after all required checks pass.
2. Complete the database and configuration rehearsal before production deployment.
3. Create the tag named in `congee/VERSION` at the reviewed commit on `main`.
4. Push that tag. The Congee workflow validates it and publishes the image to GHCR.
5. Record the image digest from the workflow. Deploy by digest, not a mutable tag.
6. Preserve the existing `/data` volume, configuration, relay identity, and admin secret.
7. Verify health, NIP-11 version, public discovery CORS, and external/local WebSocket access after deployment.
8. Test search, listing revisions, exact replays, and recipient-protected messaging with synthetic events.

No workflow deploys to Fly automatically. The release handoff must identify the operator, reviewed SHA, image digest, rehearsal evidence, and rollback image digest.

Before cutover, rehearse against an independent consistent copy of the event and metadata databases. Keep the copy and configuration private. Check schema version, FTS rowid mapping and triggers, row counts, integrity, search updates, deletion, and startup configuration compatibility. Preserve the relay identity. Do not test a replacement binary against the live database.

Stop if the rehearsal fails, the schema-8 layout differs, identity changes, required NIPs disappear, or any protected message is visible to an unauthorized reader. Reverting the image is safe only after verifying that its schema is compatible with the persisted data. Escalate an incompatible database to a maintainer.

## Rate-limit diagnostics

Use bounded aggregate counters from the authenticated admin statistics endpoint. Compare REQ throttle counts with total REQs over a defined interval. Track connection and subscription counts. Do not dump logs or configuration.

Treat logs as sensitive: they can contain IPs, pubkeys, event data, and exception details. Select only reviewed, content-free diagnostic fields before any output leaves the process. Frontend telemetry must remain content-free too.
