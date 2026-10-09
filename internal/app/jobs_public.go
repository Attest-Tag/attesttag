package app

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A fix job's engine talks to a model provider, and when the provider refuses a call the engine
// says so in the provider's own words. Qwen Code prints "[API Error: 402 This request requires
// more credits … visit https://…/keys/<the key's hash> and adjust the key's total limit]" as if it
// were its answer, and that answer becomes the job's summary. Those words name the account the call
// was made on (a workspace, a key's hash, what it can still afford), which is the deployment's
// business and nobody's on the far side of a pull request or a chat thread.
//
// Everything a job says outside the console goes through PublicJobText or publicJobError: the pull
// request, its commit, a comment on a pull request, and the Slack or Teams thread. The raw text stays
// on the job's stored result, which the console shows an organisation's admins, and in the server log.

// Why a provider refused, as a thread or a pull request is told it.
const (
	providerRefused     = "the model provider refused the request"
	providerCreditLimit = providerRefused + ": credit or key limit reached"
	providerRateLimited = providerRefused + ": rate limited"
	providerFailed      = "the model provider returned an error"
)

// providerErrorRes find a provider's refusal in a job's text, each running to the end of its line.
// They match the wrappers engines put around a failed call and the providers' own wording, never a
// bare status code or a bare phrase like "rate limit": a fix job's summary is as likely as not about
// a 429 in somebody's own code, and that sentence is not the provider's.
var providerErrorRes = []*regexp.Regexp{
	// Qwen Code, and the CLIs it comes from, print a failed call as "[API Error: 402 …]".
	regexp.MustCompile(`\[API Error:[^\n]*`),
	// Other engines print it on a line of its own: "API Error: 429 …".
	regexp.MustCompile(`(?m)^\W{0,4}API Error:\s*[45]\d\d\b[^\n]*`),
	// The worker's own words when an engine exits on a failed call: "pi exited with code 1: 429 …".
	regexp.MustCompile(`\b(?:qwen|pi) exited with code \d+:\W*[45]\d\d\b[^\n]*`),
	// The providers' wording for a refusal of credit, wrapped or not; the whole line goes.
	regexp.MustCompile(`(?i)[^\n]*(?:fewer max_tokens|but can only afford|key's total limit|insufficient credits\W+add more|please check your plan and billing details)[^\n]*`),
}

// providerCreditWords are the providers' words for a request refused for its account's credit or
// its key's limit, whatever status came with them.
var providerCreditWords = []string{"requires more credits", "can only afford", "insufficient credits", "total limit", "key limit", "exceeded your current quota", "insufficient_quota", "payment required"}

var providerRateWords = []string{"rate limit", "rate-limit", "rate_limit", "too many requests"}

var (
	// providerStatusNearRe is a status named as one: "Error: 503", "status code 429", "\"code\":402".
	providerStatusNearRe = regexp.MustCompile(`(?i)\b(?:error|status|code|http)\b[^0-9\n]{0,12}\b([45]\d\d)\b`)
	// providerStatusRe is one standing alone, as an SDK puts it first: "… code 1: 429 Rate limit".
	// Not one inside a sum or a count — "afford 412." — which ends in a full stop or a digit.
	providerStatusRe = regexp.MustCompile(`(?:^|[\s:(\[])([45]\d\d)(?:$|[\s:)\],;])`)
)

// What is left of an account's name once the refusals are gone: a link with a long hex segment in
// it, a key's hash or a workspace's id in the path, and such a string on its own.
var (
	publicLinkRe = regexp.MustCompile(`(?i)\bhttps?://[^\s<>"'\x60|)\]]+`)
	longHexRe    = regexp.MustCompile(`[0-9a-fA-F]{32,}`)
)

// PublicJobText is a job's own text (its summary, a note, the engine's log) fit for a pull request
// or a chat thread: each provider refusal in it becomes a short plain reason, and links and strings
// that could name an account are taken out of what is left.
func PublicJobText(s string) string {
	spans := providerErrorSpans(s)
	if len(spans) > 0 {
		var b strings.Builder
		at := 0
		for _, sp := range spans {
			b.WriteString(s[at:sp[0]])
			reason := providerReason(s[sp[0]:sp[1]])
			if sp[0] == 0 || s[sp[0]-1] == '\n' {
				reason = upperFirst(reason) + "."
			}
			b.WriteString(reason)
			at = sp[1]
		}
		b.WriteString(s[at:])
		s = b.String()
	}
	return scrubAccountRefs(s)
}

// publicJobError is an error a job ended with, for the same places. When a provider refused, the
// refusal is the whole of what is said, as a clause: the rest of such an error is the engine's
// output around it, which the console keeps.
func publicJobError(s string) string {
	if spans := providerErrorSpans(s); len(spans) > 0 {
		return providerReason(s[spans[0][0]:spans[0][1]])
	}
	return scrubAccountRefs(s)
}

// providerErrorSpans are where providerErrorRes match in s, in order, overlaps merged.
func providerErrorSpans(s string) [][2]int {
	var spans [][2]int
	for _, re := range providerErrorRes {
		for _, m := range re.FindAllStringIndex(s, -1) {
			spans = append(spans, [2]int{m[0], m[1]})
		}
	}
	if len(spans) < 2 {
		return spans
	}
	slices.SortFunc(spans, func(a, b [2]int) int { return a[0] - b[0] })
	out := spans[:1]
	for _, sp := range spans[1:] {
		if last := &out[len(out)-1]; sp[0] <= last[1] {
			last[1] = max(last[1], sp[1])
		} else {
			out = append(out, sp)
		}
	}
	return out
}

// providerReason names one refusal: credit, rate, or the status the provider answered with.
func providerReason(span string) string {
	low := strings.ToLower(span)
	has := func(words []string) bool {
		return slices.ContainsFunc(words, func(w string) bool { return strings.Contains(low, w) })
	}
	code := providerStatus(span)
	switch {
	case code == 402, has(providerCreditWords) && (code != 429 || !has(providerRateWords)):
		return providerCreditLimit
	case code == 429, has(providerRateWords):
		return providerRateLimited
	case code != 0:
		return providerRefused + ": provider error " + strconv.Itoa(code)
	}
	return providerFailed
}

// providerStatus is the HTTP status a refusal carries, or 0.
func providerStatus(span string) int {
	for _, re := range []*regexp.Regexp{providerStatusNearRe, providerStatusRe} {
		if m := re.FindStringSubmatch(span); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
	}
	return 0
}

func scrubAccountRefs(s string) string {
	s = publicLinkRe.ReplaceAllStringFunc(s, func(u string) string {
		if longHexRe.MatchString(u) {
			return "[link removed]"
		}
		return u
	})
	return longHexRe.ReplaceAllString(s, "[id removed]")
}

func upperFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[n:]
}
