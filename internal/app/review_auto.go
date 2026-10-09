package app

import (
	"context"
	"slices"
	"strings"

	"attesttag/internal/review"
)

// Automatic review types. A type that carries a pattern (review.Type.Auto) joins a review when the
// pull request's diff matches it, on the units of the diff that do: Concurrency and state runs on a
// change that touches async code, timers, locks or queues without every branch rule naming it. Which
// reviews may gain one is the plan's to say (reviewPlan.auto): the lane hands the engine the types
// that may join (reviewAutoTypes), and the engine, once it has read and ranked the files, adds those
// a file it will read matches (addAutoTypes). The run is then recorded, cached and summarised under
// the types it ran, the automatic ones marked "(auto)".

// reviewReleaseType is the built-in release summary's key: a review that runs it gains no automatic
// type (reviewPlan.auto).
const reviewReleaseType = "release"

// reviewAutoTypes are the types that may join a review whose own types are have: each built-in with
// a pattern that have does not already hold, as the organisation runs it — its copy, which keeps the
// built-in's pattern, or the built-in. A type the organisation turned off joins nothing, and neither
// does a type of the organisation's own that happens to hold a built-in's key: the pattern is the
// built-in's, about the built-in's rubric.
func reviewAutoTypes(ctx context.Context, st *Store, orgID int64, have []string) ([]reviewTypeSpec, error) {
	var out []reviewTypeSpec
	for _, bt := range review.BuiltinTypes() {
		if bt.Auto == "" || slices.ContainsFunc(have, func(k string) bool { return strings.EqualFold(strings.TrimSpace(k), bt.Key) }) {
			continue
		}
		row, err := st.ReviewTypeByKey(ctx, orgID, bt.Key)
		if err != nil {
			return nil, err
		}
		switch {
		case row == nil:
			out = append(out, reviewTypeSpec{Type: bt})
		case row.Enabled && row.BuiltinKey == bt.Key:
			ts := reviewTypeOfRow(row)
			ts.Auto = bt.Auto
			out = append(out, ts)
		}
	}
	return out, nil
}

// addAutoTypes adds to the run each type of spec.Auto whose pattern a reviewable file matches, after
// the spec's own types: those are what somebody chose, and when the money or the time runs short it
// is the automatic ones that go without. It runs once the files are ranked and cut, so a file the
// review will not read brings nothing in, and before the skills are read, so an automatic type's
// are read like any other's.
func (r *reviewRun) addAutoTypes() {
	for _, ts := range r.spec.Auto {
		if slices.ContainsFunc(r.spec.Types, func(have reviewTypeSpec) bool { return have.Key == ts.Key }) ||
			!slices.ContainsFunc(r.reviewable, func(f *reviewFile) bool { return reviewAutoMatches(&ts, f) }) {
			continue
		}
		ts.Automatic = true
		// Clipped, so the append copies: the spec's slice is the lane's, which it records the run under.
		r.spec.Types = append(slices.Clip(r.spec.Types), ts)
		r.out.AutoTypes = append(r.out.AutoTypes, ts)
	}
}

// reviewAutoUnits are the units holding a file that matches ts's pattern. An automatic type reads
// where its subject is rather than the whole pull request again, which is what keeps one cheap
// enough to add without anybody asking; a unit is still read whole, since the state a matching file
// shares is often in the file beside it.
func reviewAutoUnits(ts *reviewTypeSpec, units []*reviewUnit) []*reviewUnit {
	var out []*reviewUnit
	for _, u := range units {
		if slices.ContainsFunc(u.files, func(f *reviewFile) bool { return reviewAutoMatches(ts, f) }) {
			out = append(out, u)
		}
	}
	return out
}

// reviewAutoMatches reports whether f brings ts in: a source file (reviewTier's first tier) whose
// path or changed lines match ts's pattern. The words an automatic type looks for are as common in a
// lockfile ("node_modules/async"), a README ("retry the upload") or a translation ("subscription") as
// in code, and a whole pass of a type about code over those is money spent on nothing.
func reviewAutoMatches(ts *reviewTypeSpec, f *reviewFile) bool {
	return f.tier == 0 && ts.AutoMatch(f.File)
}

// reviewChosenTypes are a run's types less the ones its diff brought in: the types it was asked for,
// which is what another request for a review is compared with. The ones the diff brought in follow
// from the commits and the settings, which that comparison holds to be the same already.
func reviewChosenTypes(r *ReviewRun) []ReviewRunType {
	ck, ok := checkpointFrom(r)
	if !ok {
		return r.Types
	}
	return slices.DeleteFunc(slices.Clone(r.Types), func(t ReviewRunType) bool {
		return slices.ContainsFunc(ck.Types, func(ct reviewTypeRunJSON) bool { return ct.Auto && ct.Key == t.Key })
	})
}

// markAutoRuns marks the type runs of the types the diff brought in, for the summary to say so.
func (r *reviewRun) markAutoRuns() {
	for i := range r.out.TypeRuns {
		key := r.out.TypeRuns[i].Key
		r.out.TypeRuns[i].Auto = slices.ContainsFunc(r.out.AutoTypes, func(ts reviewTypeSpec) bool { return ts.Key == key })
	}
}
