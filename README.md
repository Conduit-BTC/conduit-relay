# Conduit L2 Scope 2 for Khatru

This directory contains the Scope 2 relay-layer extension for `khatru`.

It is organized as a self-contained module area with:

- extension implementation (`scope2.go`)
- unit and e2e tests
- runnable demo relay (`cmd/demo`)
- demo validation script (`run_demo.sh`)

## Build the demo relay

From this project root:

```bash
go build ./cmd/demo
```

## Build Docker image (for conduit-mono local stack)

From this project root:

```bash
docker build -t conduitl2:local .
```

## Deploy to Fly.io

This repository now includes a `fly.toml` tuned for a production relay rollout:

- VM preset: `performance-1x`
- Memory per Machine: `2gb`
- Fly Proxy edge handlers on `80`/`443` mapped to internal `3334`
- Persistent volume mounted at `/data`
- Relay sync enabled by default with `DATA_DIR=/data`
- Default deployed backfill horizon is recent-first, not full-history

TLS and reverse-proxy behavior:

- `wss://conduitl2.fly.dev` is served through Fly Proxy TLS termination on port `443`
- The app only listens on plain HTTP/WebSocket on `PORT=3334`; no app-level TLS config is required
- Port `80` is redirected to HTTPS

For a custom domain (for example `relay.example.com`), Fly manages certificate issuance after DNS is configured:

```bash
fly certs add relay.example.com -a conduitl2
fly certs check relay.example.com -a conduitl2
```

Apply the DNS records shown by `fly certs add` (typically `A`, `AAAA`, or `CNAME`) at your DNS provider.

Create one volume per machine before scaling out, because Fly volumes are not shared between machines:

```bash
fly volumes create data --region iad --size 1 -n 2 -a conduitl2
```

If you keep the current single-volume setup, the production recipe should explicitly stay single-machine:

```bash
fly scale count 1 -a conduitl2
fly volumes list -a conduitl2
```

If you want multiple app machines later, you need one volume per machine and each machine will build its own local cache independently.

Typical deploy flow:

```bash
fly deploy -a conduitl2 --image registry.fly.io/conduitl2:<deployment-tag>
fly scale count 2 -a conduitl2
fly scale show -a conduitl2
```

You can verify secure relay connectivity with:

```bash
nak relay wss://conduitl2.fly.dev
```

## Product cache sync

The demo relay can now preload and continuously cache public `kind:30402` products plus `kind:5` deletions from public relays.

How it works:

- Historical preload runs at startup from `SYNC_BACKFILL_SINCE`
- Default backfill horizon is `1 year` in the past, so deployed relays reach current marketplace inventory quickly without wasting days crawling from 2020
- Results are persisted in the local BoltDB event store under `DATA_DIR`
- A sync watermark is stored in `sync-state.json` under `DATA_DIR`
- After backfill, the relay keeps live subscriptions open to the configured source relays and imports new products/deletions continuously
- The relay logs each backfill window and the current watermark to stdout, so `fly logs` shows real sync progress

Default source relays:

- `wss://relay.damus.io`
- `wss://nos.lol`
- `wss://relay.primal.net`
- `wss://relay.plebeian.market`

Observed relay-set experiment for the last month of `kind:30402` events:

- `damus + nos.lol + primal`: about `940` unique product events
- Adding `relay.plebeian.market`: about `1038` unique product events
- Adding `offchain.pub` on top of that only added about `13` more unique events in the same window, so it is not enabled by default

Observed local storage sample during import:

- Last-month raw upstream payload from the default relay set was about `2.3 MB` of JSON event data
- A partial local BoltDB cache sample after `75s` of import was about `9.6 MB` while storing `69` raw `30402` events and `100` deletion events
- One-year upstream payload estimate was about `4.8 MB` for `30402` plus about `170 KB` for product-related `kind:5` deletion events
- `1GB` is therefore a comfortable starting Fly volume size for the current cache shape; auto-extension remains enabled if usage grows materially

Runtime environment variables:

- `DATA_DIR`: persistent storage directory
- `SYNC_ENABLED`: enable preload/live caching
- `SYNC_RELAYS`: comma-separated source relay URLs
- `SYNC_BACKFILL_SINCE`: RFC3339 timestamp for the initial historical import start; if unset, defaults to about 1 year ago
- `SYNC_BACKFILL_WINDOW`: duration for backfill window splitting, for example `168h`
- `SYNC_LIVE_LOOKBACK`: overlap window before live mode, for example `10m`
- `SYNC_FETCH_LIMIT`: per-window relay request limit

Example local run with sync enabled:

```bash
SYNC_ENABLED=true \
DATA_DIR=tmp/demo-data \
SYNC_RELAYS='wss://relay.damus.io,wss://nos.lol,wss://relay.primal.net,wss://relay.plebeian.market' \
go run ./cmd/demo
```

You can inspect whether products were imported with:

```bash
nak req -q -k 30402 -l 10 --search 'conduit-l2:q=;sort=newest' ws://127.0.0.1:3334
```

On Fly, useful debugging commands are:

```bash
fly logs -a conduitl2 --machine <machine-id>
fly machine exec <machine-id> "sh -lc 'ls -la /data; cat /data/sync-state.json'" -a conduitl2 --timeout 60
```

`fly logs` only shows application stdout/stderr plus platform event lines. There is not a separate hidden machine-log stream for your Go process, so sync progress needs to be logged by the app itself.

Useful Go-process log commands on Fly:

```bash
# Tail all current app stdout/stderr plus Fly platform lines
fly logs -a conduitl2

# Tail a single machine so the stream is easier to read
fly logs -a conduitl2 --machine <machine-id>

# Fetch buffered logs once, without tailing
fly logs -a conduitl2 --machine <machine-id> --no-tail

# Filter locally to only the Go process lines
fly logs -a conduitl2 --machine <machine-id> --no-tail | grep '2026/'

# Watch just sync progress lines from the Go process
fly logs -a conduitl2 --machine <machine-id> | grep '\[conduitl2-sync\]\|backfill\|live sync\|sync enabled'
```

The last command works because the sync worker now logs its own startup settings, each backfill window, and the current watermark to stdout.

If you are running a single attached volume, clean up stray unattached volumes after failed scale-out attempts:

```bash
fly volumes list -a conduitl2
fly volumes destroy <unattached-volume-id> -a conduitl2
```

## Run the demo relay

From this project root:

```bash
go run ./cmd/demo
```

Relay endpoint: `ws://127.0.0.1:3334`

## Quick `nak` showcase

1) Check relay capabilities (`conduit_l2` appears in NIP-11 `tags`):

```bash
nak relay ws://127.0.0.1:3334
curl -sS -H 'Accept: application/nostr+json' http://127.0.0.1:3334
```

2) Generate a key:

```bash
nak key generate
```

3) Publish product events (`kind:30402`):

```bash
export NOSTR_SECRET_KEY='<your nsec or hex secret key>'
nak event --sec "$NOSTR_SECRET_KEY" -k 30402 -d apple  -c '{"title":"Apple","summary":"Red fruit","price":19,"currency":"USD","updatedAt":1000}' ws://127.0.0.1:3334
nak event --sec "$NOSTR_SECRET_KEY" -k 30402 -d banana -c '{"title":"Banana","summary":"Yellow fruit","price":7,"currency":"USD","updatedAt":1001}' ws://127.0.0.1:3334
nak event --sec "$NOSTR_SECRET_KEY" -k 30402 -d carrot -c '{"title":"Carrot","summary":"Orange root","price":12,"currency":"USD","updatedAt":1002}' ws://127.0.0.1:3334
```

4) Query products with deterministic Scope 2 sort:

```bash
nak req -k 30402 -l 10 --search 'conduit-l2:q=;sort=price_asc' ws://127.0.0.1:3334
```

Price sorting is only allowed when the matched products are comparable. Mixed-currency results require either trusted normalization or `partial=1`.

5) Query products with text search:

```bash
nak req -k 30402 -l 10 --search 'conduit-l2:q=apple;sort=newest' ws://127.0.0.1:3334
```

6) Query a mixed-currency set with deterministic partial results:

```bash
nak req -k 30402 -l 10 --search 'conduit-l2:q=;sort=price_asc;partial=1' ws://127.0.0.1:3334
```

7) Show fail-closed behavior for an unscoped `kind:1059` read:

```bash
nak req -k 1059 ws://127.0.0.1:3334
```

This request is intentionally invalid: gift-wrap reads require NIP-42 authentication and an exact recipient filter. A valid client flow is:

1. Handle the relay's `AUTH` challenge.
2. Sign and send a kind `22242` authentication event containing the exact relay URL and challenge.
3. Wait for the successful `OK`, then retry the read as `{"kinds":[1059],"#p":["<authenticated 32-byte lowercase hex pubkey>"]}`.
4. Authorization follows the most recently successful identity on that WebSocket. Existing protected subscriptions are re-checked on every delivery after an identity change.

Unfiltered, wildcard, mixed-kind, malformed, multi-recipient, and mismatched-recipient gift-wrap reads are rejected or suppressed. Kindless subscriptions are rejected because backend result limits could otherwise expose protected activity even after event filtering. Protected counts and protected negentropy are deliberately unsupported. Public subscriptions remain available when they explicitly request public kinds.

The product's relay-read loop must implement the challenge/sign/send/wait/retry sequence above before this relay policy is rolled out. Deploy the product support first, then the relay, then verify stored and live A/B isolation in production. Do not describe a deployment as recipient-restricted until that verification passes.

Deleted product revisions are filtered out of accelerated browse results. This package currently advertises browse/search/sort behavior plus recipient-authorized `kind:1059` reads; it does not advertise separate product-detail or profile-lookup acceleration.

## One-command demo and validation

From this project root:

```bash
./run_demo.sh
```

The script starts the demo relay, publishes test products, validates sorting and text filtering, and checks that an unauthenticated/unscoped `kind:1059` request fails closed.
