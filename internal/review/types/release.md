# Release review: for a large merge into a production branch, such as a release or
# promotion branch merging into main. Summary first; inline comments for P0 only.
#
# Format: see general.md and ParseType in types.go.

key: release
name: Release summary
strictness: high
inline: P0
purpose: Say what is shipping and what has to happen for it to ship safely. Most of the
changes in a merge like this were reviewed in their own pull requests, so the summary is the
main result: what changed, area by area, and what the person deploying it must know. Only a
problem that would cause an outage, data loss or a security hole once deployed is worth a
comment on the diff; report everything else briefly, for the summary.

- [P2] Summarise what ships by area, such as the API, the database, the user interface,
  background jobs and infrastructure, in a sentence or two each, naming the pull requests or
  commits behind each change when they can be seen.
- [P0; paths: **/migrations/**, **/migrate/**, **/*.sql, **/schema.*] Database migrations:
  list each one, and flag any that drops or renames a column or table that code on either
  side of the deploy still reads, rewrites or locks a large table, or cannot be rolled back.
- [P1] Configuration: new, renamed or removed environment variables, feature flags, secrets
  and config keys, and where each has to be set before the deploy.
- [P1] Deploy order: changes that only work if another service, a migration, a worker or a
  client ships first or at the same time. Say the order.
- [P1] Old and new versions running side by side during the rollout: a message, job, cache
  entry, cookie or API response that one version writes and the other cannot read.
- [P0] Anything in the release that would cause data loss, an outage or a security hole once
  deployed, with the file and the line.
- [P1] Dependency upgrades in the release, with major-version bumps and lockfile changes
  called out.
- [P2] Risky files: the changed files most likely to break production, such as
  authentication, billing, migrations, concurrency and infrastructure, so whoever merges
  knows where to look first.
- [P2] Rollback: whether deploying the previous version again undoes the release, and what
  would stop it, such as a migration, a one-way change to data or messages already sent.
- [P2] Changes in this merge that did not come through a reviewed pull request of their own,
  when that can be seen from the commits.
