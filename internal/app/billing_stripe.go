package app

// The Stripe client. Named billing_stripe.go and not stripe.go deliberately: presets.go already
// defines a connection preset called "stripe", which is a CUSTOMER's own restricted key routed
// through the proxy so the bot can answer questions about their charges. That Stripe is somebody
// else's account and this one is ours, and confusing the two would be the worst possible bug in
// either file.
//
// Hand-rolled rather than the vendor SDK, for the reasons resendMailer and aws_sigv4.go were:
// go.mod is deliberately short, the surface here is four operations of which three are a
// form-encoded POST, and the SDK's value is typed objects across two hundred resources when we
// want a handful of fields from six event payloads. The one genuinely fiddly part is the
// signature, and it is a pure function below with a test per failure mode.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// stripeAPIVersion is pinned so that a change to Stripe's account default cannot alter the shape
// of a REPLY this code parses. Moving it is a deliberate act with the changelog open.
//
// It does NOT govern webhook bodies, and believing it did cost an afternoon. An event is rendered
// in the API version of the endpoint it is delivered to — set when that endpoint was created,
// from the account default of that day — so a deployment can be reading 2026-08-26.dahlia events
// through a parser written for the version above and be told nothing. Three fields moved in
// 2025-04-30.basil; billing.go parses both spellings rather than pinning what it cannot pin.
const stripeAPIVersion = "2024-06-20"

// stripeTolerance is how far a webhook's timestamp may be from this clock. Five minutes is
// Stripe's own default, and it is checked in both directions: a timestamp from the future is as
// good a sign of a forged or replayed request as a stale one.
const stripeTolerance = 5 * time.Minute

// stripeBodyLimit bounds a webhook body before it is read, the way slackBodyLimit does. The
// signature covers the bytes, so they have to be read whole — which is exactly why there has to
// be a ceiling on how many of them.
const stripeBodyLimit = 1 << 20

// Payments is the payment provider, as an interface for the same reason Mailer is one: the tests
// have to be able to take money without a network, and a deployment with no keys has to degrade
// rather than fail to start.
type Payments interface {
	Configured() bool
	Checkout(ctx context.Context, req CheckoutRequest) (CheckoutSession, error)
	Portal(ctx context.Context, req PortalRequest) (string, error)
	// Session reads a Checkout Session back. It is what lets the console settle a payment on the
	// way back from Stripe instead of waiting for the webhook, which removes the race rather than
	// papering over it.
	Session(ctx context.Context, id string) (CheckoutSession, error)
	CancelSubscription(ctx context.Context, id string) error
	// ChangeSubscriptionPrice moves a live subscription onto another Price — the one operation
	// behind changing size. It replaces the single item rather than adding a second, and what it
	// does about the difference depends on which way the move goes: see PriceChange.
	ChangeSubscriptionPrice(ctx context.Context, ch PriceChange) (PriceChangeResult, error)
	// PendingPriceChange reads a size move Stripe is holding until the period ends, so the
	// console can say which size and which date. Read rather than remembered: a copy on this
	// side would be one more thing to drift, and this one is about money.
	PendingPriceChange(ctx context.Context, subscriptionID string) (PendingChange, error)
	// Price reads one Price. It is how an enterprise deal's price is checked when the operator
	// names it, rather than when the customer presses Subscribe and meets a typo.
	Price(ctx context.Context, id string) (PriceInfo, error)
	// VerifyWebhook checks the signature over the exact bytes Stripe signed and returns the
	// envelope. raw is the body before anything has decoded it; header is Stripe-Signature.
	VerifyWebhook(raw []byte, header string, at time.Time) (StripeEvent, error)
}

type CheckoutRequest struct {
	Mode          string // "subscription" | "payment"
	CustomerID    string // reuse cus_… when this organisation has one
	CustomerEmail string // only when it does not
	PriceID       string // subscription mode
	Quantity      int64
	AmountMinor   int64 // payment mode: the top-up, in minor units, as Stripe counts
	Currency      string
	Description   string
	ClientRef     string // the organisation's public id; Stripe echoes it as client_reference_id
	Metadata      map[string]string
	SuccessURL    string
	CancelURL     string
	// IdempotencyKey stops a double-clicked button from opening two sessions. It is not the
	// thing that stops a double CREDIT — that is credit_ledger.external_id, keyed on the payment
	// itself, because two sessions can still be paid.
	IdempotencyKey string
}

type CheckoutSession struct {
	ID  string
	URL string
	// Status is Stripe's open | complete | expired. Only a complete session has been paid for (or
	// needed no payment), so it is the one the settle-on-return shortcut may apply.
	Status         string
	CustomerID     string
	Mode           string
	PaymentStatus  string
	PaymentIntent  string
	SubscriptionID string
	AmountMinor    int64
	Currency       string
	ClientRef      string
	Created        int64 // unix seconds; the settle-on-return path refuses a stale session
	Metadata       map[string]string
}

type PortalRequest struct{ CustomerID, ReturnURL string }

// PriceChange is one move of a live subscription onto another Price.
type PriceChange struct {
	SubscriptionID string
	PriceID        string
	// ChargeNow invoices the difference and takes the money on the spot, instead of letting it
	// ride the next invoice. Only an upgrade ever sets it — a downgrade's difference is a
	// credit, and there is nothing to take. sizeChangeCharges is what decides.
	ChargeNow bool
	// IdempotencyKey has to be DERIVED from the move rather than random, because once an upgrade
	// takes money on the spot it is the only thing standing between a double-clicked button and
	// two charges. The handler's "already on this size" guard is not that thing: it reads a
	// record this side writes only AFTER Stripe has answered, so two requests in flight together
	// both sail past it and both charge.
	IdempotencyKey string
}

// PriceChangeResult is what actually happened, which is not always what was asked for.
type PriceChangeResult struct {
	// Charged is whether money moved just now. A downgrade never charges, and an upgrade that
	// fell exactly on a renewal may not either, so nothing downstream may announce a payment
	// without reading this first.
	Charged     bool
	AmountMinor int64
	Currency    string
	// InvoiceURL is the hosted invoice, when there is one worth showing a customer.
	InvoiceURL string
	// Scheduled is the downgrade case: nothing moved and nothing was charged, because the
	// customer keeps the size they have paid for until the period ends. Pending says what
	// happens at the boundary and when.
	Scheduled bool
	Pending   PendingChange
	// Released is a pending change this call called off — somebody changing their mind about a
	// downgrade before the date it was to happen, or upgrading instead.
	Released bool
}

// PendingChange is a size move Stripe is holding until the current period ends.
type PendingChange struct {
	ScheduleID string
	PriceID    string // the price it moves onto
	AtUnix     int64  // when it does
}

// Waiting reports whether there is a move to tell the customer about.
func (p PendingChange) Waiting() bool { return p.PriceID != "" && p.AtUnix > 0 }

// StripeEvent is the envelope, with the object left raw. Each handler unmarshals the shape it
// needs; decoding two hundred resources to reach six fields is what the SDK is for.
type StripeEvent struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Created  int64  `json:"created"`
	Livemode bool   `json:"livemode"`
	Data     struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

// ---- the real one ----

type stripeClient struct {
	key, whsec string
	currency   string
	base       string // overridden in tests; api.stripe.com otherwise
	client     *http.Client
}

func (c *stripeClient) Configured() bool { return true }

// post is every write this client makes: form-encoded, bearer-authenticated, version-pinned, and
// carrying an idempotency key so a retry is free.
func (c *stripeClient) post(ctx context.Context, path string, form url.Values, idem string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", c.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	c.auth(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	return c.do(req, out)
}

func (c *stripeClient) get(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+path, nil)
	if err != nil {
		return err
	}
	c.auth(req)
	return c.do(req, out)
}

func (c *stripeClient) auth(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Stripe-Version", stripeAPIVersion)
}

// stripeError is a refusal with Stripe's own code kept alongside its message. The code is the
// part that matters: two ways of not taking a payment need two different answers, and a string
// is no way to tell them apart.
type stripeError struct {
	Status  int
	Code    string
	Message string
}

func (e *stripeError) Error() string {
	return fmt.Sprintf("the payment provider refused it (%d): %s", e.Status, e.Message)
}

// needsCardAction reports a card asking for its owner — 3-D Secure, or any other challenge only
// the cardholder can answer. It is not a decline and retrying will not help: something has to
// put the challenge in front of a human, and an API call cannot.
func needsCardAction(err error) bool {
	var se *stripeError
	return errors.As(err, &se) && se.Code == "subscription_payment_intent_requires_action"
}

// cardWasDeclined reports a card that said no. Worth telling apart from every other refusal
// because the answer is a different card, not a retry or a support conversation.
func cardWasDeclined(err error) bool {
	var se *stripeError
	return errors.As(err, &se) && se.Code == "card_declined"
}

func (c *stripeClient) do(req *http.Request, out any) error {
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach the payment provider: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode >= 300 {
		// Stripe's own message is the useful half — a price that does not exist, a customer in
		// the wrong mode — and it is what a support conversation will actually be about.
		var e struct {
			Error struct{ Message, Code string } `json:"error"`
		}
		json.Unmarshal(raw, &e)
		return &stripeError{Status: resp.StatusCode, Code: e.Error.Code,
			Message: nonEmpty(e.Error.Message, truncate(string(raw), 200))}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// stripeSession is the subset of a Checkout Session this code reads.
type stripeSession struct {
	ID            string            `json:"id"`
	URL           string            `json:"url"`
	Status        string            `json:"status"`
	Mode          string            `json:"mode"`
	Customer      string            `json:"customer"`
	PaymentStatus string            `json:"payment_status"`
	PaymentIntent string            `json:"payment_intent"`
	Subscription  string            `json:"subscription"`
	AmountTotal   int64             `json:"amount_total"`
	Currency      string            `json:"currency"`
	ClientRef     string            `json:"client_reference_id"`
	Created       int64             `json:"created"`
	Metadata      map[string]string `json:"metadata"`
}

func (s stripeSession) toSession() CheckoutSession {
	return CheckoutSession{ID: s.ID, URL: s.URL, Status: s.Status, Mode: s.Mode, CustomerID: s.Customer,
		PaymentStatus: s.PaymentStatus, PaymentIntent: s.PaymentIntent, SubscriptionID: s.Subscription,
		AmountMinor: s.AmountTotal, Currency: s.Currency, ClientRef: s.ClientRef, Created: s.Created, Metadata: s.Metadata}
}

func (c *stripeClient) Checkout(ctx context.Context, r CheckoutRequest) (CheckoutSession, error) {
	f := url.Values{}
	f.Set("mode", r.Mode)
	f.Set("success_url", r.SuccessURL)
	f.Set("cancel_url", r.CancelURL)
	if r.ClientRef != "" {
		f.Set("client_reference_id", r.ClientRef)
	}
	if r.CustomerID != "" {
		f.Set("customer", r.CustomerID)
	} else if r.CustomerEmail != "" {
		f.Set("customer_email", r.CustomerEmail)
	}
	// Metadata goes on the session AND on the object the later events carry. Stripe does not
	// propagate one to the other, and forgetting this is the single most common way a top-up
	// arrives as a payment nobody can attribute to an account.
	for k, v := range r.Metadata {
		f.Set("metadata["+k+"]", v)
		switch r.Mode {
		case "subscription":
			f.Set("subscription_data[metadata]["+k+"]", v)
		case "payment":
			f.Set("payment_intent_data[metadata]["+k+"]", v)
		}
	}
	switch r.Mode {
	case "subscription":
		qty := r.Quantity
		if qty <= 0 {
			qty = 1
		}
		f.Set("line_items[0][price]", r.PriceID)
		f.Set("line_items[0][quantity]", strconv.FormatInt(qty, 10))
	case "payment":
		f.Set("line_items[0][quantity]", "1")
		f.Set("line_items[0][price_data][currency]", r.Currency)
		f.Set("line_items[0][price_data][unit_amount]", strconv.FormatInt(r.AmountMinor, 10))
		f.Set("line_items[0][price_data][product_data][name]", nonEmpty(r.Description, "API credit"))
		// Keep the customer Stripe creates, so the next top-up and the billing portal land on
		// the same one rather than scattering an account across several. Only when there is
		// nobody to keep: Stripe rejects the pair outright ("you may only specify one of these
		// parameters: customer, customer_creation"), so an account that already has a customer
		// — every account that has subscribed or topped up once — cannot ask for another.
		if r.CustomerID == "" {
			f.Set("customer_creation", "always")
		}
	default:
		return CheckoutSession{}, fmt.Errorf("unknown checkout mode %q", r.Mode)
	}
	var s stripeSession
	if err := c.post(ctx, "/v1/checkout/sessions", f, r.IdempotencyKey, &s); err != nil {
		return CheckoutSession{}, err
	}
	return s.toSession(), nil
}

func (c *stripeClient) Session(ctx context.Context, id string) (CheckoutSession, error) {
	var s stripeSession
	if err := c.get(ctx, "/v1/checkout/sessions/"+url.PathEscape(id), &s); err != nil {
		return CheckoutSession{}, err
	}
	return s.toSession(), nil
}

func (c *stripeClient) Portal(ctx context.Context, r PortalRequest) (string, error) {
	f := url.Values{}
	f.Set("customer", r.CustomerID)
	f.Set("return_url", r.ReturnURL)
	var out struct {
		URL string `json:"url"`
	}
	if err := c.post(ctx, "/v1/billing_portal/sessions", f, "", &out); err != nil {
		return "", err
	}
	return out.URL, nil
}

func (c *stripeClient) CancelSubscription(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "DELETE", c.base+"/v1/subscriptions/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	c.auth(req)
	return c.do(req, nil)
}

// ChangeSubscriptionPrice swaps the price of a subscription's only item.
//
// Two calls, because the write has to name the item it is replacing: giving Stripe a price with
// no item id ADDS a line, and the customer would then be billed for both sizes. Reading the
// subscription back first is also what makes "the subscription has gone" a clear error here
// rather than a confusing charge.
// stripeSubscription is the subset of a subscription these operations read. schedule is the
// one that matters here: a non-empty one means a size move is already waiting at the boundary.
type stripeSubscription struct {
	ID       string            `json:"id"`
	Status   string            `json:"status"`
	Schedule string            `json:"schedule"`
	Metadata map[string]string `json:"metadata"`
	Items    struct {
		Data []struct {
			ID       string `json:"id"`
			Quantity int64  `json:"quantity"`
			Price    struct {
				ID string `json:"id"`
			} `json:"price"`
		} `json:"data"`
	} `json:"items"`
}

type stripePhase struct {
	StartDate int64 `json:"start_date"`
	EndDate   int64 `json:"end_date"`
	Items     []struct {
		Price    string `json:"price"`
		Quantity int64  `json:"quantity"`
	} `json:"items"`
}

type stripeSchedule struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	Subscription string `json:"subscription"`
	CurrentPhase struct {
		StartDate int64 `json:"start_date"`
		EndDate   int64 `json:"end_date"`
	} `json:"current_phase"`
	Phases []stripePhase `json:"phases"`
}

// PriceInfo is the part of a Price an enterprise deal needs: whether it can be subscribed to, what
// it charges, and how often.
type PriceInfo struct {
	ID         string
	Active     bool
	Currency   string
	UnitAmount int64 // minor units; 0 for a tiered price, which has no single figure
	Recurring  bool
	Interval   string // "month", "year", …; empty for a one-off price
	IntervalN  int64
	Tiered     bool
}

type stripePrice struct {
	ID            string `json:"id"`
	Active        bool   `json:"active"`
	Currency      string `json:"currency"`
	UnitAmount    int64  `json:"unit_amount"`
	Type          string `json:"type"`
	BillingScheme string `json:"billing_scheme"`
	Recurring     *struct {
		Interval      string `json:"interval"`
		IntervalCount int64  `json:"interval_count"`
	} `json:"recurring"`
}

func (c *stripeClient) Price(ctx context.Context, id string) (PriceInfo, error) {
	var p stripePrice
	if err := c.get(ctx, "/v1/prices/"+url.PathEscape(id), &p); err != nil {
		return PriceInfo{}, err
	}
	out := PriceInfo{ID: p.ID, Active: p.Active, Currency: p.Currency, UnitAmount: p.UnitAmount,
		Recurring: p.Type == "recurring" && p.Recurring != nil, Tiered: p.BillingScheme == "tiered"}
	if p.Recurring != nil {
		out.Interval, out.IntervalN = p.Recurring.Interval, p.Recurring.IntervalCount
	}
	return out, nil
}

func (c *stripeClient) subscription(ctx context.Context, id string) (stripeSubscription, error) {
	var sub stripeSubscription
	err := c.get(ctx, "/v1/subscriptions/"+url.PathEscape(id), &sub)
	return sub, err
}

// ChangeSubscriptionPrice moves a subscription's only item onto another Price. Which way the
// move goes decides almost everything, so this is mostly a fork:
//
//   - Up is owed now. The difference for the rest of a month already paid for is invoiced and
//     taken on the spot, and the size changes as the payment lands.
//   - Down is not owed at all. The customer paid for this month and keeps it — the size, and
//     everything it includes, until the period ends. The move is handed to a Stripe schedule
//     that applies it at the boundary. Nothing is charged, nothing is credited, and nothing
//     about the account changes today.
//
// Two calls in either case, because the write has to name the item it is replacing: giving
// Stripe a price with no item id ADDS a line, and the customer would then be billed for both
// sizes. Reading the subscription back first is also what makes "the subscription has gone" a
// clear error here rather than a confusing charge.
func (c *stripeClient) ChangeSubscriptionPrice(ctx context.Context, ch PriceChange) (PriceChangeResult, error) {
	sub, err := c.subscription(ctx, ch.SubscriptionID)
	if err != nil {
		return PriceChangeResult{}, err
	}
	if len(sub.Items.Data) == 0 {
		return PriceChangeResult{}, errors.New("that subscription has no items to change")
	}
	item := sub.Items.Data[0]

	// Already on the size being asked for. Not an error — a stale tab and a double click both
	// land here — but it is also the only way somebody calls off a downgrade before the date it
	// was to happen, so it cannot simply be answered from this side.
	if item.Price.ID == ch.PriceID {
		if sub.Schedule == "" {
			return PriceChangeResult{}, nil
		}
		if err := c.releaseSchedule(ctx, sub.Schedule); err != nil {
			return PriceChangeResult{}, err
		}
		return PriceChangeResult{Released: true}, nil
	}

	if !ch.ChargeNow {
		return c.scheduleAtPeriodEnd(ctx, ch, sub)
	}

	// An upgrade happens now, so a downgrade sitting in a schedule is off — they have just
	// chosen something else. Released first, or the schedule would pull the price back down at
	// the boundary and quietly undo the size that was paid for a moment ago.
	released := false
	if sub.Schedule != "" {
		if err := c.releaseSchedule(ctx, sub.Schedule); err != nil {
			return PriceChangeResult{}, err
		}
		released = true
	}
	res, err := c.chargeNow(ctx, ch, item.ID)
	res.Released = released
	return res, err
}

// chargeNow is the upgrade: invoice the difference and take it.
func (c *stripeClient) chargeNow(ctx context.Context, ch PriceChange, itemID string) (PriceChangeResult, error) {
	f := url.Values{}
	f.Set("items[0][id]", itemID)
	f.Set("items[0][price]", ch.PriceID)
	f.Set("proration_behavior", "always_invoice")
	// error_if_incomplete is what makes charging on the spot safe: a card that cannot pay
	// leaves the subscription exactly where it was, rather than moving the size and leaving an
	// unpaid invoice behind it.
	f.Set("payment_behavior", "error_if_incomplete")
	// Expanded so the amount actually taken comes back with the write, rather than needing a
	// second round trip to find out what was just charged.
	f.Set("expand[]", "latest_invoice")
	var out struct {
		LatestInvoice struct {
			AmountPaid int64  `json:"amount_paid"`
			Currency   string `json:"currency"`
			HostedURL  string `json:"hosted_invoice_url"`
			Status     string `json:"status"`
		} `json:"latest_invoice"`
	}
	if err := c.post(ctx, "/v1/subscriptions/"+url.PathEscape(ch.SubscriptionID), f, ch.IdempotencyKey, &out); err != nil {
		return PriceChangeResult{}, err
	}
	// Read the subscription back rather than believing the write's own answer. A replayed
	// idempotent request returns the FIRST request's body, describing a move that this time did
	// not happen — the one way this could report success over an unchanged subscription, and
	// the one that would leave our record and Stripe's disagreeing about what is being billed.
	after, err := c.subscription(ctx, ch.SubscriptionID)
	if err != nil {
		return PriceChangeResult{}, err
	}
	if len(after.Items.Data) == 0 || after.Items.Data[0].Price.ID != ch.PriceID {
		return PriceChangeResult{}, errors.New("the payment provider did not move the subscription; nothing has changed — wait a minute and try again")
	}
	res := PriceChangeResult{Currency: out.LatestInvoice.Currency, InvoiceURL: out.LatestInvoice.HostedURL}
	// Charged is read off what was PAID, not off what was asked for: an upgrade landing on a
	// renewal can invoice nothing at all, and saying "we charged you" then would be a lie.
	if out.LatestInvoice.AmountPaid > 0 {
		res.Charged, res.AmountMinor = true, out.LatestInvoice.AmountPaid
	}
	return res, nil
}

// scheduleAtPeriodEnd is the downgrade: two phases, the size they are paying for until the
// period ends and the smaller one after it, with nothing moving today.
//
// Any schedule already attached is released and a fresh one built from the subscription as it
// now stands, rather than editing phases in place. It costs one call and it means phase 0 is
// always exactly what the customer is living on — which is what makes changing your mind about
// which size to drop to, twice, behave the same as doing it once.
func (c *stripeClient) scheduleAtPeriodEnd(ctx context.Context, ch PriceChange, sub stripeSubscription) (PriceChangeResult, error) {
	released := false
	if sub.Schedule != "" {
		if err := c.releaseSchedule(ctx, sub.Schedule); err != nil {
			return PriceChangeResult{}, err
		}
		released = true
	}
	var sched stripeSchedule
	f := url.Values{}
	f.Set("from_subscription", ch.SubscriptionID)
	if err := c.post(ctx, "/v1/subscription_schedules", f, idemSuffix(ch.IdempotencyKey, "sched"), &sched); err != nil {
		return PriceChangeResult{}, err
	}
	if len(sched.Phases) == 0 || len(sched.Phases[0].Items) == 0 {
		return PriceChangeResult{}, errors.New("the payment provider returned a schedule with no phase to follow")
	}
	now := sched.Phases[0]
	if now.EndDate <= 0 {
		return PriceChangeResult{}, errors.New("the payment provider did not say when the current period ends, so there is no date to move on")
	}

	// Phase 0 is echoed back exactly as Stripe just described it. It is the month already paid
	// for and it must not be touched — rewriting it is how a "downgrade next month" turns into
	// a charge today.
	u := url.Values{}
	u.Set("end_behavior", "release")
	u.Set("proration_behavior", "none")
	u.Set("phases[0][start_date]", strconv.FormatInt(now.StartDate, 10))
	u.Set("phases[0][end_date]", strconv.FormatInt(now.EndDate, 10))
	u.Set("phases[0][items][0][price]", now.Items[0].Price)
	u.Set("phases[0][items][0][quantity]", strconv.FormatInt(max64(now.Items[0].Quantity, 1), 10))
	u.Set("phases[1][items][0][price]", ch.PriceID)
	u.Set("phases[1][items][0][quantity]", strconv.FormatInt(max64(now.Items[0].Quantity, 1), 10))
	// One cycle on the new size, then the schedule lets go and the subscription carries on by
	// itself. A schedule that never releases would own every later size change too.
	u.Set("phases[1][iterations]", "1")
	// Carried onto both phases explicitly. A phase's metadata becomes the subscription's when
	// that phase is entered, so leaving it off would quietly empty the subscription's metadata
	// at the boundary — taking attesttag_org with it. Nothing would break loudly, because an
	// event can still be traced through its customer, but the fallback that exists for Stripe's
	// unordered delivery would be gone, and it would be gone months after the change that did
	// it.
	for k, v := range sub.Metadata {
		u.Set("phases[0][metadata]["+k+"]", v)
		u.Set("phases[1][metadata]["+k+"]", v)
	}
	var updated stripeSchedule
	if err := c.post(ctx, "/v1/subscription_schedules/"+url.PathEscape(sched.ID), u,
		idemSuffix(ch.IdempotencyKey, "phase"), &updated); err != nil {
		return PriceChangeResult{}, err
	}
	// Read back, for the reason the upgrade does: a replayed write answers with the first
	// request's body. Here that would leave a customer told their size drops next month while
	// nothing at Stripe intends to drop it, and they find out by being charged the larger fee.
	pending, err := c.PendingPriceChange(ctx, ch.SubscriptionID)
	if err != nil {
		return PriceChangeResult{}, err
	}
	if pending.PriceID != ch.PriceID {
		return PriceChangeResult{}, errors.New("the payment provider did not record the change for the end of the period; nothing has changed — wait a minute and try again")
	}
	return PriceChangeResult{Scheduled: true, Released: released, Pending: pending}, nil
}

// releaseSchedule lets the subscription go on by itself, cancelling whatever the schedule was
// going to do to it. Release rather than cancel: cancelling a schedule can cancel the
// subscription under it, which is emphatically not what calling off a downgrade means.
func (c *stripeClient) releaseSchedule(ctx context.Context, id string) error {
	return c.post(ctx, "/v1/subscription_schedules/"+url.PathEscape(id)+"/release", url.Values{}, "", nil)
}

func (c *stripeClient) PendingPriceChange(ctx context.Context, subscriptionID string) (PendingChange, error) {
	sub, err := c.subscription(ctx, subscriptionID)
	if err != nil {
		return PendingChange{}, err
	}
	if sub.Schedule == "" {
		return PendingChange{}, nil
	}
	var sched stripeSchedule
	if err := c.get(ctx, "/v1/subscription_schedules/"+url.PathEscape(sub.Schedule), &sched); err != nil {
		return PendingChange{}, err
	}
	return sched.pending(), nil
}

// pending is the phase after the one running now — the move being waited for. Read from the
// phases rather than assumed to be the last one, because a schedule that has already rolled
// over has its spent phases still listed behind it.
func (s stripeSchedule) pending() PendingChange {
	boundary := s.CurrentPhase.EndDate
	if boundary <= 0 {
		return PendingChange{}
	}
	for _, p := range s.Phases {
		if p.StartDate >= boundary && len(p.Items) > 0 && p.Items[0].Price != "" {
			return PendingChange{ScheduleID: s.ID, PriceID: p.Items[0].Price, AtUnix: p.StartDate}
		}
	}
	return PendingChange{}
}

// idemSuffix keeps the calls of one size change on distinct keys. Stripe refuses a key reused
// with different parameters, so the several writes a scheduled downgrade makes cannot share the
// one the move derives.
func idemSuffix(key, part string) string {
	if key == "" {
		return ""
	}
	return key + ":" + part
}

func (c *stripeClient) VerifyWebhook(raw []byte, header string, at time.Time) (StripeEvent, error) {
	if err := verifyStripeSignature(raw, header, c.whsec, at, stripeTolerance); err != nil {
		return StripeEvent{}, err
	}
	var e StripeEvent
	if err := json.Unmarshal(raw, &e); err != nil {
		return StripeEvent{}, fmt.Errorf("the webhook body is not an event: %w", err)
	}
	if e.ID == "" || e.Type == "" {
		return StripeEvent{}, errors.New("the webhook body has no event id or type")
	}
	return e, nil
}

// ---- the signature ----

// verifyStripeSignature checks one Stripe-Signature header against the exact bytes of the body.
//
// The signed message is "<timestamp>.<raw body>". The `t=` in the header is a FIELD NAME, not part
// of what was signed — a client that includes it computes a MAC over the wrong message, so it
// verifies nothing and refuses every genuine delivery while looking like it works. That is the
// mistake this function exists to not make, and there is a test named after it.
//
// The key is the whsec_… string itself, used as raw bytes, not decoded.
//
// Several v1= values may appear at once: that is how a webhook secret is rotated without an
// outage, so any one of them matching is a pass. hmac.Equal, never ==, so a wrong signature takes
// the same time as a right one.
func verifyStripeSignature(raw []byte, header, secret string, at time.Time, tolerance time.Duration) error {
	if secret == "" {
		return errors.New("no webhook secret is configured")
	}
	if header == "" {
		return errors.New("no Stripe-Signature header")
	}
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if ts == "" {
		return errors.New("the signature header has no timestamp")
	}
	if len(sigs) == 0 {
		return errors.New("the signature header has no v1 signature")
	}
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("the signature timestamp is not a number")
	}
	// Both directions. A future timestamp is not a clock being generous, it is a signature that
	// would stay valid for as long as whoever made it decided.
	if drift := at.Sub(time.Unix(secs, 0)); drift > tolerance || drift < -tolerance {
		return fmt.Errorf("the signature timestamp is %s away from now, outside the %s tolerance",
			drift.Round(time.Second), tolerance)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(raw)
	want := mac.Sum(nil)
	for _, s := range sigs {
		got, err := hex.DecodeString(s)
		if err != nil {
			continue
		}
		if hmac.Equal(got, want) {
			return nil
		}
	}
	return errors.New("no signature in the header matches the body")
}

// signStripePayload is the same computation the other way round. It exists so the tests can make
// a genuine delivery instead of asserting against a constant somebody pasted, and so the
// reconciliation sweep can replay one.
func signStripePayload(raw []byte, secret string, at time.Time) string {
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(raw)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// ---- no keys ----

// offPayments is what runs on every deployment that sells nothing. It reports itself
// unconfigured, and the routes are never registered in the first place, so nothing should ever
// reach these — the errors are the belt to that braces.
type offPayments struct{}

func (offPayments) Configured() bool { return false }
func (offPayments) Checkout(context.Context, CheckoutRequest) (CheckoutSession, error) {
	return CheckoutSession{}, errors.New("this deployment does not sell plans")
}
func (offPayments) Portal(context.Context, PortalRequest) (string, error) {
	return "", errors.New("this deployment does not sell plans")
}
func (offPayments) Session(context.Context, string) (CheckoutSession, error) {
	return CheckoutSession{}, errors.New("this deployment does not sell plans")
}
func (offPayments) CancelSubscription(context.Context, string) error {
	return errors.New("this deployment does not sell plans")
}
func (offPayments) ChangeSubscriptionPrice(context.Context, PriceChange) (PriceChangeResult, error) {
	return PriceChangeResult{}, errors.New("this deployment does not sell plans")
}
func (offPayments) PendingPriceChange(context.Context, string) (PendingChange, error) {
	return PendingChange{}, nil
}
func (offPayments) Price(context.Context, string) (PriceInfo, error) {
	return PriceInfo{}, errors.New("this deployment does not sell plans")
}
func (offPayments) VerifyWebhook([]byte, string, time.Time) (StripeEvent, error) {
	return StripeEvent{}, errors.New("this deployment does not sell plans")
}

// NewPayments picks one. Like NewMailer it never refuses to start: a deployment with no Stripe
// keys is not a broken deployment, it is the ordinary one.
func NewPayments(cfg Config) Payments {
	if !cfg.BillingEnabled() {
		return offPayments{}
	}
	slog.Info("billing is on", "sizes", len(cfg.StripeSizes), "currency", cfg.BillingCurrency,
		"mode", stripeMode(cfg.StripeSecretKey))
	return &stripeClient{key: cfg.StripeSecretKey, whsec: cfg.StripeWebhookSecret,
		currency: cfg.BillingCurrency, base: "https://api.stripe.com",
		client: &http.Client{Timeout: 25 * time.Second}}
}

// stripeMode is "live" or "test", read from the key's own prefix. It is logged at boot and
// compared against every event's livemode, which is what catches a test webhook pointed at
// production before it credits somebody with money nobody paid.
//
// A restricted key (rk_) names its mode exactly as a secret key (sk_) does. Reading only sk_live_
// put a deployment running on a restricted live key in test mode, and the livemode check then
// acknowledged every real event and acted on none: a customer paid and was never credited.
func stripeMode(key string) string {
	if strings.HasPrefix(key, "sk_live_") || strings.HasPrefix(key, "rk_live_") {
		return "live"
	}
	return "test"
}
