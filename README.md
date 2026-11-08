# conduit-relay

Production Congee and the retired Conduit L2 reference implementation live in
separate directories.

## Production — `congee/`

The production relay builds from `congee/` via `congee/Dockerfile`, with its
plugin SDK in the nested `congee/sdk/plugin/` module. Source provenance,
validation, release, and deployment steps live in
[Congee source and releases](docs/CONGEE_SOURCE.md).

## Retired — `legacy/`

`legacy/` contains the deprecated khatru relay, Scope 2 product browsing, and
NIP-42 gift-wrap protection. Keep it as reference and for local reproduction;
do not deploy it to new apps. Its historical Dockerfile and Fly configuration
are under `legacy/`, so the repository root is not a legacy deployment context.
See the [legacy README](legacy/README.md).

## CI and review


Pull requests run one `build` check: Congee, its admin UI, the plugin SDK,
and the legacy relay must build. Focused recipient-protection, legacy deletion/sync recovery tests, and the
public-workflow credential boundary run in that same job. Superseded runs cancel.

Full tests, formatting, vet, race checks, PostgreSQL regressions, and container
smoke run through the manually dispatched **Release validation** workflow and
on `congee-v*` tags. Only validated release tags publish images. See
[Congee source and releases](docs/CONGEE_SOURCE.md) before releasing.

Review is maintainer-requested. There is no PR-title gate or automatic second
review pass. Keep account credentials and review App private keys out of public
workflows.

CI also checks that external actions and reusable workflows use full commit SHAs,
and workflow containers use image digests. Local composite actions are included.
Run `ruby scripts/check-action-pins-test.rb` and `ruby scripts/check-action-pins.rb`
to check changes locally; Ruby and its bundled YAML parser require no extra gems.
Keep version comments beside pins for review and dependency updates. This checks
repository-owned YAML, not runtime downloads or external actions' internals.

## Manual pull request reviews


The previous automated review runner and its `/agent review` and
`/agent simplify` commands are retired. Reviews now use the Devin GitHub
integration and are requested manually:

- PR review: an authorized maintainer with a linked Devin account can comment
  `/devin review` on an open pull request.
- Final simplicity review: invoke the organization's Ponytail skill in Devin
  with `@skills:ponytail-final <PR URL>`, or in the Devin CLI with
  `/ponytail:ponytail-final <PR URL>` after syncing organization plugins.
  Use this advisory pass after correctness feedback has been addressed.

Automatic review and autoapproval rules are disabled. Ponytail runs only on
explicit request. Human review and required CI remain required.

Review credentials must remain outside public workflows. Review output does
not authorize code changes, approval, merge, or deployment.
