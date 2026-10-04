# Security review: who can do or see what they should not, because of this change.
#
# Format: see general.md and ParseType in types.go.

key: security
name: Security
strictness: medium
inline: P2
purpose: Find what lets somebody do or see what they should not: missing authentication or
authorisation, data crossing between tenants, injection, leaked secrets, requests the server
makes to places a user chooses, and changes to dependencies and CI that widen what runs with
the repository's secrets. Treat every value that arrives from a request, a file, a webhook,
a queue or another service as controlled by an attacker until the code checks it. Report a
hole only when you can say who could exploit it, from where, and what they would get.

- [P0] An endpoint, handler, job or RPC that acts on a resource without checking that the
  caller is signed in and allowed to act on that particular resource, or a check that was
  removed, weakened or moved after the action it guards.
- [P0] Tenant isolation: a query, cache key, file path, search or queue over data that belongs
  to many tenants (organisations, accounts, workspaces) that is not filtered by the caller's
  tenant, or that takes the tenant from the request instead of the session.
- [P0] Injection: untrusted input put into SQL, a shell command, a template rendered without
  escaping, another query language, a regular expression or a file path, instead of being
  passed as a parameter or checked against an allowlist.
- [P0] Secrets: credentials, tokens, private keys or connection strings committed to code,
  config, fixtures or docs, or written to logs, error messages, URLs or anything sent to the
  browser. Never repeat the secret's value in a finding.
- [P0] Server-side request forgery: the server fetching a URL or host a user can influence
  without an allowlist, including through redirects, names that resolve to internal
  addresses, link previews and webhook targets.
- [P0] Unsafe deserialisation or evaluation of untrusted data: pickle, YAML loaders that build
  objects, native object deserialisers, eval, new Function, or loading a module, class or
  template a user names.
- [P1] Open redirects: redirecting to a URL or path taken from the request without checking
  that it stays on an allowed origin.
- [P1] Cross-site scripting: untrusted data written into HTML, an attribute, a URL or a script
  without the framework's escaping, or through an escape hatch such as
  dangerouslySetInnerHTML, v-html, innerHTML or a template's raw or safe filter.
- [P1] Cookies and sessions: an auth cookie without Secure, HttpOnly and a suitable SameSite;
  a session that is not rotated at sign-in or on a change of privilege; tokens or reset links
  that never expire or can be used twice.
- [P1] CORS: any origin allowed together with credentials, the Origin header reflected back,
  or origins matched by substring, prefix or an unanchored pattern.
- [P1] Cryptography: a home-made scheme, ECB mode, fixed or reused IVs and nonces, a fast
  hash for passwords, MD5 or SHA-1 for signatures, secrets or MACs compared with ordinary
  equality, or predictable random values used as tokens.
- [P1] Webhooks and callbacks accepted without verifying their signature, verified only after
  the payload has been acted on, or verified with a comparison that is not constant time.
- [P1] Mass assignment: a request body bound straight onto a model or an update, so a caller
  can set fields such as role, owner, tenant, price or verified.
- [P1; paths: **/package.json, **/package-lock.json, **/yarn.lock, **/pnpm-lock.yaml, **/go.mod, **/go.sum, **/requirements*.txt, **/pyproject.toml, **/poetry.lock, **/uv.lock, **/Pipfile.lock, **/Gemfile, **/Gemfile.lock, **/Cargo.toml, **/Cargo.lock, **/pom.xml, **/build.gradle*, **/composer.json, **/composer.lock]
  Dependency changes: a new dependency, a major-version bump, a changed registry or download
  URL, an install script, or a lockfile that changed without its manifest. Say what changed
  and what the new code is now able to do.
- [P0; paths: .github/**, .gitlab-ci.yml, .circleci/**, **/Jenkinsfile, azure-pipelines.yml, .buildkite/**]
  CI changes: pull_request_target or workflow_run jobs that run the pull request's code with
  secrets or a write token, broader token permissions, third-party actions not pinned to a
  commit, secrets passed to steps that run untrusted code, or event fields such as titles
  and branch names expanded inside a shell script.
- [P2] Missing rate or size limits on endpoints that send email or messages, create accounts,
  check passwords or one-time codes, or accept uploads.
- [P2] Personal data, tokens or whole request bodies written to logs or sent to third-party
  services such as analytics or error trackers without being redacted.
