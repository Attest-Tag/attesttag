package app

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// One channel setting, checked once. PUT /api/scopes/{id} and the console assistant's staging
// tool both come through here, because a rule written down twice is a rule that holds in one
// place — and the assistant's whole promise is that a proposal it offers is one the save will
// accept. A proposal validated against a looser copy of these rules would put a Confirm button
// in front of somebody and then fail under their hand.
//
// Three of these checks are new, and each replaces a silence rather than a refusal:
// monthly_budget_usd threw its parse error away and stored 0, so "lots" quietly switched the
// channel's budget off; member_edits was never checked at all, and memberEditsBlocked reads an
// unrecognised value as "inherit", so a typo looked saved and did nothing; read_all and
// email_intake are coerced to "inherit" in the store, so "yes" reported success and changed
// nothing. All three are worse with a model composing the values.

// scopeFields are the settings PUT /api/scopes/{id} understands, with the console's own words
// for each. The label is what a person reads on a proposal card, so it matches the channel's
// settings page rather than the column name.
var scopeFields = map[string]string{
	"instructions":       "Custom instructions",
	"default_model":      "Default model",
	"member_edits":       "Member edits",
	"read_all":           "Read every message",
	"email_intake":       "Email intake",
	"email_auto_writes":  "Allow rules on forwarded email",
	"monthly_budget_usd": "Monthly budget (USD)",
	"max_tool_rounds":    "Maximum tool rounds",
	"allow_rules":        "Auto mode allow rules",
	"default_repo":       "Default repository",
}

// scopeFieldLabel names a field the way the console does, falling back to the wire name so an
// unlabelled field still reads as something rather than as nothing.
func scopeFieldLabel(key string) string {
	if l, ok := scopeFields[key]; ok {
		return l
	}
	return key
}

// threeWay is the inherit/on/off vocabulary shared by read_all and email_intake, and the
// allow/block one member_edits uses. Spelled as a set rather than a switch so the error can
// list what was expected.
func threeWay(value string, a, b string) error {
	switch strings.TrimSpace(value) {
	case "inherit", "", a, b:
		return nil
	}
	return fmt.Errorf("must be one of inherit, %s or %s", a, b)
}

// scopeFieldError checks one posted channel setting the way the save enforces it. A key it does
// not know returns nil: the endpoint has always ignored extras, and the assistant refuses an
// unknown field separately against proposableScopeFields, so tightening it here would break a
// caller that sends something harmless without protecting anything.
//
// The model catalogue is deliberately not consulted. A write must not become conditional on the
// provider being reachable; modelOffered is the assistant's own, stricter check.
func (b *Bot) scopeFieldError(ctx context.Context, orgID int64, sc *Scope, key, value string) error {
	fail := func(err error) error { return fmt.Errorf("%s: %w", scopeFieldLabel(key), err) }
	switch key {
	case "instructions", "default_model":
		return nil
	case "member_edits":
		if err := threeWay(value, "allow", "block"); err != nil {
			return fail(err)
		}
	case "read_all", "email_intake", "email_auto_writes":
		if err := threeWay(value, "on", "off"); err != nil {
			return fail(err)
		}
	case "monthly_budget_usd":
		v := strings.TrimSpace(value)
		if v == "" {
			return nil // an emptied field is "no budget", as every other scope setting reads it
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return fail(fmt.Errorf("must be a number of dollars, got %q", value))
		}
		if f < 0 {
			return fail(fmt.Errorf("cannot be negative"))
		}
	case "max_tool_rounds":
		v := strings.TrimSpace(value)
		if v == "" {
			return nil // "inherit"
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fail(fmt.Errorf("must be a whole number; 0 inherits"))
		}
	case "allow_rules":
		if _, err := parseAllowRules(value); err != nil {
			return fail(err)
		}
	case "default_repo":
		repo, err := normalizeRepo(value)
		if err != nil {
			return fail(err)
		}
		if repo == "" || sc == nil {
			return nil
		}
		// Reachability, not spelling: a repository the channel cannot open is a default that
		// fails on the first turn that needs it. -1 bypasses the config-version cache, which is
		// how the save asks the same question.
		acc, _ := b.resolver.Resolve(ctx, orgID, sc.TeamID, sc.SlackID, -1)
		if acc == nil || !acc.hasRepo(repo) {
			return fmt.Errorf("%s is not connected here; connect it first", repo)
		}
	}
	return nil
}

// modelOffered says whether the provider still lists this model. Only the assistant asks: a
// proposal naming a model nobody can serve is worth refusing before a card offers it, while a
// save typed by a person is not worth making conditional on the catalogue being reachable.
//
// An unreachable catalogue is not a refusal. The alternative is an assistant that stops working
// whenever the provider's model list is slow, which is a worse failure than letting one
// unrecognised id through to a save that would have taken it anyway.
func (b *Bot) modelOffered(ctx context.Context, orgID int64, id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return true // "inherit"
	}
	if id == "heavy" {
		return b.settings.Get(ctx, orgID).HeavyModel != ""
	}
	// The organisation's own endpoint when it brought one: that is where the model will be asked
	// for, and a model the deployment's endpoint serves may not exist there.
	l, err := b.agent.llmFor(ctx, orgID)
	if err != nil || l == nil {
		return true
	}
	models, err := l.ListModels(ctx, false)
	if err != nil || len(models) == 0 {
		return true
	}
	for _, m := range models {
		if strings.EqualFold(m.ID, id) {
			return true
		}
	}
	return false
}
