# Release process

Releases go out every Tuesday at 10:00 Kathmandu time. The release captain for the week creates the release branch on Monday, runs the full regression suite, and posts the changelog draft in #releases.

## Hotfixes
A hotfix can ship any day if it fixes a customer-facing outage. It needs one approval from a senior engineer and a note in #engineering.

## Rollback
If error rates rise above 2% within an hour of deploy, roll back with `deploy rollback prod` and open an incident in #incidents.
