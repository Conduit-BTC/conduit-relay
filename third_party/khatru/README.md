# Pinned Khatru security patch

This directory is the Khatru package from `fiatjaf.com/nostr` commit
`d43fbbf02d92` (the repository's original pinned dependency), isolated as a
local package so the required relay-framework fixes can be reviewed independently:

- `OnAuth` runs synchronously while the connection authentication lock is held
  and before the relay sends the successful AUTH response.
- AUTH events must have a canonical transmitted event ID in addition to the
  pinned NIP-42 validator's signature and tag checks; content is unrestricted.
- authentication getters read an immutable atomic snapshot instead of racing
  the AUTH handler's exported compatibility slice.
- live broadcast and listener inspection use a per-relay listener lock,
  including routed subrelays, instead of racing registration and removal.
- alternate forced broadcasts still apply `PreventBroadcast` so authorization
  cannot be bypassed by a second live-delivery API.
- rejected multi-filter requests remove listeners admitted by earlier filters,
  and empty-filter requests cancel their request context.
- non-author deletion attempts do not distinguish protected event IDs from
  nonexistent IDs.

These changes are required for recipient-authorized kind-1059 delivery. Keep
this fork narrow and remove it when upstream exposes equivalent ordering and
synchronization without regressing `go test -race ./...`.
