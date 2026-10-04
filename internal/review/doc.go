// Package review holds the judgements of code review that need nothing but their inputs:
// reading a pull request's diff and deciding which lines a GitHub comment may sit on, what a
// finding is and when two findings are the same one, the score, which branch rule a pull
// request falls under, how settings inherit down a connection's tree, what a comment
// addressed to the bot is asking for, the MAC-signed markers that identify our own comments,
// the sanitiser every model-written word passes before GitHub renders it, the rendering of
// inline comments and the summary, and the built-in review types and the prompt section that
// presents a type's rubric to the finder.
//
// It imports only the standard library and does no I/O, on purpose. Almost everything here is
// a decision about text somebody else controls — a diff, a PR comment, a model's findings — or
// one that GitHub punishes with a 422 when it is wrong, and a package with no database,
// network or clock is one a table of cases can pin down completely. internal/app does the
// fetching, storing and posting, and calls in here for every one of these decisions, so the
// engine, the console API and the tests all get the same answer.
package review
