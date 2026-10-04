-- What the review lane needs to carry a run from the moment it is queued to the moment its review
-- is on GitHub, which 0027's columns did not hold.
--
--   review_runs.installation_id
--                     the GitHub App installation the trigger came through. Every token the run
--                     mints is minted from it, and the run is re-checked against it when it is
--                     claimed: the tables hold the repository, never which installation reaches it,
--                     and a run queued a minute before the installation was removed from the review
--                     tree must find that out before it spends anything.
--   review_runs.request_json
--                     what was asked for beyond the kind and the trigger: whether to post live or
--                     only record (a person may ask for shadow on a live repository), whether the
--                     whole pull request or only what changed since the last review, and whether
--                     the types were named by a person rather than chosen by a branch rule — so a
--                     rule edited while the run waits does not replace what somebody asked for.
--   review_runs.outcome_json
--                     the part of the engine's result the summary is rendered from and no other
--                     column holds: each type's summary or why it did not run, whether every line
--                     was read, whether the diff spoke to the reviewer, the file hashes, and the
--                     context repositories with the commits they were read at. Written in one
--                     transaction with the run's findings, fenced on its lease. That is the
--                     checkpoint: a run requeued between its model work and its post — GitHub's
--                     rate limit, a lane that died — posts from it and never pays for the model
--                     twice, and a resync renders the summary again from it with no model at all.
--   review_findings.possibly_outdated
--                     1 for a finding on a file that changed after the commit it was found on,
--                     before it could be posted: it is kept off the diff and listed in the
--                     summary under "Possibly outdated", until a later review sees it again.
--   review_findings.place
--                     where the engine put the finding, in its own words: inline, outside_diff,
--                     masked (a hunk a credential was masked in), unchanged_file, pre_existing,
--                     more_notes (over the review's caps) or below_inline_severity. placement says
--                     only whether it went on the diff; this is which heading of the summary lists
--                     one that did not, so overflow is not passed off as "outside the diff".
--
-- And one correction to 0027, which is applied and so is not edited: review_prs.file_hashes holds
-- review.PatchHash per path — a hash of the +/- lines of that file's change — and not the blob sha
-- 0027 describes. A re-review compares the change, not the file, so a file whose diff is the same
-- after a rebase counts as unchanged.
alter table review_runs add column installation_id integer not null default 0;
alter table review_runs add column request_json text not null default '{}';
alter table review_runs add column outcome_json text not null default '{}';
alter table review_findings add column possibly_outdated integer not null default 0;
alter table review_findings add column place text not null default '';
