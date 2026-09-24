// Package guide carries this repository's documentation into the binary.
//
// It exists because the console assistant has to answer "how do I set this up" questions, and
// the answers are here rather than in any database: how a connection's path prefixes and methods
// are written, what "allow access grants" changes about where a write goes, what an approval tier
// grants, what a plan costs. None of that is in the organisation's own data, so no amount of
// reading the console tells you it — and a model asked without it answers plausibly and wrongly,
// which is worse than not answering, because the reader will go and try what it said.
//
// A Go file in a documentation folder looks like a mistake and is not: //go:embed patterns cannot
// reach outside their own directory, so the alternative is copying the markdown into the package
// at build time and having the copy go stale.
package guide

import "embed"

// Files is every page of the guide, as written. Nothing generates or rewrites these — the file
// the maintainer edits is the file the assistant quotes.
//
//go:embed *.md
var Files embed.FS
