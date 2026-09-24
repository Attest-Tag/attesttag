package app

import (
	"html"
	"regexp"
	"strconv"
	"strings"
)

// attest_tag writes in Slack's dialect everywhere: <@U123> for a person, <#C123> for a channel,
// <https://x|label> for a link, *bold* in the text it composes itself. That is its internal wire
// format — twenty-odd files build it and the console's own renderer reads it — so it is not
// rewritten at each of those places. It is rewritten once, here, on the way out to Teams.

// msMention is one person a Teams message mentions. Teams draws "<at>Name</at>" in the text as a
// mention only when an entity names who it is.
type msMention struct {
	Type      string    `json:"type"`
	Text      string    `json:"text"`
	Mentioned msAccount `json:"mentioned"`
}

var (
	slackLinkRe    = regexp.MustCompile(`<((?:https?://|mailto:)[^\s|>]+)(?:\|([^>]*))?>`)
	slackChannelRe = regexp.MustCompile(`<#([A-Za-z0-9:@._;=-]+)(?:\|([^>]*))?>`)
	slackMentionRe = regexp.MustCompile(`<@([A-Za-z0-9-]+)(?:\|([^>]*))?>`)
	slackSpecialRe = regexp.MustCompile(`<!(here|channel|everyone)(?:\|[^>]*)?>`)
	mrkdwnBoldRe   = regexp.MustCompile(`(^|[^*\w])\*([^*\n]+)\*([^*\w]|$)`)
	mrkdwnStrikeRe = regexp.MustCompile(`(^|[^~\w])~([^~\n]+)~([^~\w]|$)`)
)

// msRenderer turns the internal dialect into what Teams draws. who names a person by id and says
// how Teams addresses them; channel names a conversation. Either may come back empty, and the text
// then carries the plain name or nothing rather than a mention that points at no one.
type msRenderer struct {
	who     func(id string) (name, teamsID string)
	channel func(id string) string
}

// markdown renders text for a Teams message. mrkdwn says the text is attest_tag's own Slack
// markup rather than an answer the model wrote in Markdown, and has its emphasis converted too.
func (r msRenderer) markdown(text string, mrkdwn bool) (string, []msMention) {
	var mentions []msMention
	seen := map[string]bool{}
	text = convertOutsideCode(text, func(s string) string {
		s = slackLinkRe.ReplaceAllStringFunc(s, func(m string) string {
			sub := slackLinkRe.FindStringSubmatch(m)
			if sub[2] == "" || sub[2] == sub[1] {
				return sub[1]
			}
			return "[" + sub[2] + "](" + sub[1] + ")"
		})
		s = slackChannelRe.ReplaceAllStringFunc(s, func(m string) string {
			sub := slackChannelRe.FindStringSubmatch(m)
			name := sub[2]
			if name == "" && r.channel != nil {
				name = r.channel(sub[1])
			}
			if name == "" {
				return "this channel"
			}
			return strings.TrimPrefix(name, "#")
		})
		s = slackSpecialRe.ReplaceAllString(s, "everyone")
		s = slackMentionRe.ReplaceAllStringFunc(s, func(m string) string {
			sub := slackMentionRe.FindStringSubmatch(m)
			name, teamsID := sub[2], ""
			if r.who != nil {
				n, tid := r.who(sub[1])
				teamsID = tid
				if name == "" {
					name = n
				}
			}
			if name == "" {
				return "someone"
			}
			if teamsID == "" {
				return name
			}
			tag := "<at>" + escapeTeamsHTML(name) + "</at>"
			if !seen[teamsID] {
				seen[teamsID] = true
				mentions = append(mentions, msMention{Type: "mention", Text: tag, Mentioned: msAccount{ID: teamsID, Name: name}})
			}
			return tag
		})
		if mrkdwn {
			s = mrkdwnBoldRe.ReplaceAllString(s, "$1**$2**$3")
			s = mrkdwnStrikeRe.ReplaceAllString(s, "$1~~$2~~$3")
		}
		return s
	})
	return text, mentions
}

// convertOutsideCode applies f to the parts of text that are not code, so a URL or an asterisk
// inside a code block is shown as it was written.
func convertOutsideCode(text string, f func(string) string) string {
	var b strings.Builder
	for text != "" {
		fence := strings.Index(text, "```")
		tick := strings.Index(text, "`")
		if tick < 0 {
			b.WriteString(f(text))
			break
		}
		start, marker := tick, "`"
		if fence >= 0 && fence == tick {
			marker = "```"
		}
		end := strings.Index(text[start+len(marker):], marker)
		if end < 0 { // an unclosed tick is text like any other
			b.WriteString(f(text))
			break
		}
		end += start + len(marker) + len(marker)
		b.WriteString(f(text[:start]))
		b.WriteString(text[start:end])
		text = text[end:]
	}
	return b.String()
}

// The keys a card button's press carries back. Namespaced, because a press arrives as an ordinary
// message whose value is whatever the card said, and the bot must know a press of its own card from
// anything else that has a value.
const (
	msCardAction = "attest_action"
	msCardValue  = "attest_value"
	// msCardSummary is what the card said first. A Teams press carries only the button's data,
	// and answering a card keeps its words while its buttons go, so the words ride along. They
	// only ever change what the answered card shows: what runs is decided by the id in the value,
	// and whether this presser may run it, both checked on arrival.
	msCardSummary = "attest_summary"
)

// adaptiveCard draws a Card as an Adaptive Card, in the order a Slack card has: the words, the
// buttons, then the small print. The buttons are an ActionSet in the body rather than the card's
// own actions, which Teams draws below everything and would put the footer above them.
func (r msRenderer) adaptiveCard(c Card) map[string]any {
	body := []any{}
	var mentions []msMention
	text := func(s string, small bool) map[string]any {
		md, ms := r.markdown(s, true)
		mentions = append(mentions, ms...)
		tb := map[string]any{"type": "TextBlock", "text": md, "wrap": true}
		if small {
			tb["size"], tb["isSubtle"] = "Small", true
		}
		return tb
	}
	for _, p := range c.Parts {
		body = append(body, text(p.Markdown, p.Small))
	}
	summary := ""
	if len(c.Parts) > 0 {
		summary = truncate(c.Parts[0].Markdown, 1500)
	}
	if len(c.Buttons) > 0 {
		actions := make([]any, 0, len(c.Buttons))
		for _, b := range c.Buttons {
			if b.URL != "" {
				actions = append(actions, map[string]any{"type": "Action.OpenUrl", "title": b.Label, "url": b.URL})
				continue
			}
			a := map[string]any{"type": "Action.Submit", "title": b.Label,
				"data": map[string]any{msCardAction: b.ActionID, msCardValue: b.Value, msCardSummary: summary}}
			if b.Primary {
				a["style"] = "positive"
			}
			actions = append(actions, a)
		}
		body = append(body, map[string]any{"type": "ActionSet", "actions": actions})
	}
	if c.Footer != "" {
		body = append(body, text(c.Footer, true))
	}
	card := map[string]any{
		"type": "AdaptiveCard", "$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"version": "1.4", "body": body,
	}
	if len(mentions) > 0 {
		card["msteams"] = map[string]any{"entities": dedupeMentions(mentions)}
	}
	return card
}

func dedupeMentions(ms []msMention) []msMention {
	seen := map[string]bool{}
	out := ms[:0:0]
	for _, m := range ms {
		if !seen[m.Mentioned.ID] {
			seen[m.Mentioned.ID] = true
			out = append(out, m)
		}
	}
	return out
}

// escapeTeamsHTML is for a name placed inside <at>…</at>, which Teams reads as markup.
func escapeTeamsHTML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// ---- incoming ----

var (
	teamsAtRe = regexp.MustCompile(`(?s)<at[^>]*>(.*?)</at>`)
	// A tag is < and a letter or a slash. "a < b > c" is somebody's sentence, not markup.
	teamsTagRe  = regexp.MustCompile(`</?[A-Za-z][^>]*>`)
	teamsCRLFRe = regexp.MustCompile(`\r\n?`)
)

// fromTeams turns an incoming Teams message into the internal dialect: the bot's own mention is
// dropped — it is only the address on the envelope — and every other mention becomes <@id>, so the
// rest of the app reads a Teams message exactly as it reads a Slack one. mentioned maps the text
// inside each <at> tag to the person's id, and a name that maps to nobody stays as plain text.
//
// What a person typed can never become markup. Slack guarantees that by escaping < and > in the
// text it delivers, which is why typing "<@U123>" there mentions nobody; the same holds here,
// because every < or > left once Teams' own tags are gone is escaped the way Slack would have.
func fromTeams(text string, botNames map[string]bool, mentioned map[string]string) string {
	text = strings.ReplaceAll(text, "\x00", "") // the placeholders below are the only NULs allowed
	text = teamsCRLFRe.ReplaceAllString(text, "\n")
	var held []string
	text = teamsAtRe.ReplaceAllStringFunc(text, func(m string) string {
		name := strings.TrimSpace(teamsAtRe.FindStringSubmatch(m)[1])
		out := ""
		switch {
		case botNames[name]:
		case mentioned[name] != "":
			out = "<@" + mentioned[name] + ">"
		default:
			out = strings.NewReplacer("<", "&lt;", ">", "&gt;").Replace(html.UnescapeString(name))
		}
		held = append(held, out)
		return "\x00" + strconv.Itoa(len(held)-1) + "\x00"
	})
	text = strings.NewReplacer("<br>", "\n", "<br/>", "\n", "<br />", "\n", "</p>", "\n", "</div>", "\n").Replace(text)
	text = teamsTagRe.ReplaceAllString(text, "")
	text = strings.NewReplacer("&nbsp;", " ", "&quot;", `"`, "&#39;", "'", "&apos;", "'").Replace(text)
	text = strings.NewReplacer("<", "&lt;", ">", "&gt;").Replace(text)
	text = teamsHeldRe.ReplaceAllStringFunc(text, func(m string) string {
		i, err := strconv.Atoi(strings.Trim(m, "\x00"))
		if err != nil || i < 0 || i >= len(held) {
			return ""
		}
		return held[i]
	})
	return strings.TrimSpace(text)
}

var teamsHeldRe = regexp.MustCompile("\x00[0-9]+\x00")
