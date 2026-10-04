package app

import (
	"context"
	"net/http"
	"strings"
)

// Which organisations have code review: CODE_REVIEW, read once at boot.
//
//   - all, the default: every organisation. A self-host is one organisation, spending its own key,
//     with no plan to gate anything on.
//   - pro: organisations on the pro or the enterprise plan — what the hosted service runs
//     (deploy/gcp/cloudrun.sh), where a review is minutes of the operator's model on somebody's
//     pull request and the free plan's few dollars a month would be one of them.
//   - enterprise: the enterprise plan only.
//   - off: nobody. The feature is not on this deployment at all: its console routes answer 404, the
//     webhook keeps no pull-request event, and no review lane runs.
//
// An organisation without it has nothing new start spending: no review as a pull request opens or
// is pushed to, no command answered but one line a day saying so, no reply in a thread answered,
// nothing started from the console. What it already had stays as it was — its settings, its history,
// the summaries on its pull requests, which a free resync still keeps true as findings change and as a
// push's delivery moves the head — so moving an account back onto a plan with code review picks up
// where it left off. One thing is not kept true meanwhile: a push whose delivery never arrived (the
// deployment was down) is not caught up, since the catch-up does not list an organisation without
// code review (reviewCatchupOrg), so the footer goes on naming the old head until the next push's
// delivery, or until the plan is back and the catch-up finds the head moved.
const (
	CodeReviewAll        = "all"
	CodeReviewPro        = "pro"
	CodeReviewEnterprise = "enterprise"
	CodeReviewOff        = "off"
)

// codeReviewOf reads CODE_REVIEW. Unset is the default, all; anything else it does not recognise is
// pro, the restrictive answer, as orgModelKeysOf narrows a typo and planOf reads an unknown plan as
// free — and LoadConfig says so at startup rather than letting a typo decide in silence. Read as all,
// a typo on the hosted service (CODE_REVIEW=pr0) would open reviews to every free account there,
// spent on the operator's model key; read as pro, a self-host with one sees the console's "needs the
// Pro plan" strip and the boot log, and puts it right.
func codeReviewOf(v string) string {
	switch v = strings.ToLower(strings.TrimSpace(v)); v {
	case "", CodeReviewAll:
		return CodeReviewAll
	case CodeReviewPro, CodeReviewEnterprise, CodeReviewOff:
		return v
	}
	return CodeReviewPro
}

// codeReviewOff reports whether this deployment has no code review at all.
func (c Config) codeReviewOff() bool { return c.CodeReview == CodeReviewOff }

// codeReviewAccess is whether an organisation has code review here, and why not when it does not:
// Reason "off" is the deployment's policy, "plan" the organisation's plan, and Needs the plan that
// would have it. The console reads it from /api/me to explain an empty Reviews page instead of
// leaving a team to wonder why nothing is reviewed; the server decides, as pausedBy does for spend,
// so the console never works the rule out for itself.
type codeReviewAccess struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
	Needs     string `json:"needs,omitempty"`
	Message   string `json:"message,omitempty"`
}

// codeReviewFor is codeReviewAccess for an organisation on plan. A Config built in code rather than
// loaded has no policy, and reads as the default.
func (c Config) codeReviewFor(plan string) codeReviewAccess {
	switch c.CodeReview {
	case CodeReviewOff:
		return codeReviewAccess{Reason: "off", Message: "Code review is not available on this deployment."}
	case CodeReviewPro:
		if plan == PlanPro || plan == PlanEnterprise {
			return codeReviewAccess{Available: true}
		}
		return codeReviewAccess{Reason: "plan", Needs: PlanPro,
			Message: "Code review is part of the Pro and Enterprise plans; this organisation is on the " + planName(plan) + " plan."}
	case CodeReviewEnterprise:
		if plan == PlanEnterprise {
			return codeReviewAccess{Available: true}
		}
		return codeReviewAccess{Reason: "plan", Needs: PlanEnterprise,
			Message: "Code review is part of the Enterprise plan here; this organisation is on the " + planName(plan) + " plan."}
	}
	return codeReviewAccess{Available: true}
}

// planName is a plan as a sentence names it.
func planName(plan string) string {
	switch planOf(plan) {
	case PlanPro:
		return "Pro"
	case PlanEnterprise:
		return "Enterprise"
	}
	return "Free"
}

// reviewAccess is codeReviewFor for one organisation, on its plan as the settings cache has it — a
// plan the operator changes is in force within the cache's fifteen seconds, as ORG_MODEL_KEYS is.
func (b *Bot) reviewAccess(ctx context.Context, orgID int64) codeReviewAccess {
	if b.cfg.CodeReview == CodeReviewAll || b.cfg.CodeReview == "" {
		return codeReviewAccess{Available: true} // no plan to read: every organisation has it
	}
	if b.settings == nil {
		return b.cfg.codeReviewFor("")
	}
	return b.cfg.codeReviewFor(b.settings.Get(ctx, orgID).Plan)
}

// reviewPlanGate is the plan at the lane's gate: nil when the organisation has code review, else the
// skip that says why not. Asked wherever spending could start — the gate every review goes through
// (enqueueReview) and its claim, a command, a reply in a thread, the console's starts — and quietly:
// the reason is recorded on the pull request as "plan", and nothing is posted for it but a command's
// one line a day.
func (b *Bot) reviewPlanGate(ctx context.Context, orgID int64) *reviewSkip {
	if a := b.reviewAccess(ctx, orgID); !a.Available {
		return &reviewSkip{Reason: "plan", Detail: a.Message}
	}
	return nil
}

// reviewPlanRefused answers a console request that would start a review for an organisation whose
// plan has no code review, and reports whether it did: 402, since what would let it is a plan the
// account has not bought, with the reason and the plan that would have it for the console to say.
// On a deployment where code review is off the routes answer 404 before this is reached (reviewOn).
func (b *Bot) reviewPlanRefused(w http.ResponseWriter, r *http.Request) bool {
	a := b.reviewAccess(r.Context(), orgOf(r))
	if a.Available {
		return false
	}
	writeJSON(w, http.StatusPaymentRequired, map[string]any{"error": a.Message, "reason": a.Reason, "needs": a.Needs})
	return true
}

// reviewOn wraps a console route of code review's: on a deployment where it is off the route is not
// there, and answers like one that is not, before any permission is asked about.
func (b *Bot) reviewOn(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if b.cfg.codeReviewOff() {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "code review is not available on this deployment", "reason": "off"})
			return
		}
		h(w, r)
	}
}
