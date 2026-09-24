package app

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The channel Configure page (linked from every reply's footer) lets channel members adjust
// how the bot behaves in their channel, without console access: read every message,
// channel instructions, a read-only view of tools and connections, and the channel's
// routines. The link carries an HMAC of the channel id under MASTER_KEY, so only people who
// can see the channel (and therefore the footer) have it. Admins can lock it per scope with
// member_edits = block.

func (a *Agent) configureURL(ctx context.Context, orgID int64, teamID, channel string) string {
	// ADMIN_BASE_URL, else the origin admins use the console on, else this process itself
	// (a laptop run: the link then works from the machine the bot runs on).
	base := publicBaseURL(ctx, a.store, a.cfg)
	if base == "" {
		return ""
	}
	var epoch int64
	if a.store != nil {
		if sc, _ := a.store.ChannelScope(ctx, orgID, teamID, channel); sc != nil {
			epoch = sc.LinkEpoch
		}
	}
	tok := mintConfigureToken(teamID, channel, "", epoch, time.Now())
	if tok == "" {
		return ""
	}
	return fmt.Sprintf("%s/configure/%s/%s?t=%s", base, teamID, channel, tok)
}

// personalConfigureURL is the same page opened as one person, which is what makes the Personal
// tab appear. It is never posted in a channel: the footer link is seen by everybody who can read
// the reply, and this one carries the authority to read and rewrite somebody's own settings.
func (a *Agent) personalConfigureURL(ctx context.Context, orgID int64, teamID, channel, slackUserID string) string {
	if slackUserID == "" {
		return ""
	}
	base := publicBaseURL(ctx, a.store, a.cfg)
	if base == "" {
		return ""
	}
	var epoch int64
	if a.store != nil {
		if sc, _ := a.store.ChannelScope(ctx, orgID, teamID, channel); sc != nil {
			epoch = sc.LinkEpoch
		}
	}
	tok := mintConfigureToken(teamID, channel, slackUserID, epoch, time.Now())
	if tok == "" {
		return ""
	}
	return fmt.Sprintf("%s/configure/%s/%s?t=%s&tab=personal", base, teamID, channel, tok)
}

// localBaseURL turns a listen address into a URL that works on the same machine: ":8090",
// "0.0.0.0:8090" and "[::]:8090" become http://localhost:8090; a bare port works too; "" yields "".
func localBaseURL(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	host, port := addr, ""
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host, port = addr[:i], addr[i+1:]
	} else if strings.Trim(addr, "0123456789") == "" {
		host, port = "", addr
	}
	host = strings.Trim(host, "[]")
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	if port == "" {
		return "http://" + host
	}
	return "http://" + host + ":" + port
}

// A Configure link is a bearer capability: whoever holds it can rewrite what the bot is told
// in that channel. It used to be a static MAC of the channel — printed under every reply,
// good forever, revocable only by rotating the key that also seals every credential. Now each
// link expires a day after the reply that carried it, and the channel's scope holds an epoch
// that an admin can bump to void every link printed so far. The MAC covers the workspace as
// well as the channel (channel ids are unique to a workspace, so a token for one team's C0ABC
// must not open another team's C0ABC), and it runs under a key derived for this purpose alone.
//
// Token shape: v1.<expiry unix>.<epoch>.<nonce>.<mac>.
const configureLinkTTL = 24 * time.Hour

// configureMAC covers the workspace, the channel and — for a link minted for one person — who
// that person is. The channel link is posted in the footer of every reply and is shared by
// everyone who can see it; a personal link is DMed to one person and is the only thing that may
// open their own settings, so the user id has to be inside the MAC rather than beside it.
//
// The two live in separate domains so a shared link can never be read as a personal one: the
// bodies already differ ("v1." against "v2."), and the prefix says so a second time.
func configureMAC(teamID, channel, user, body string) string {
	key := derivedKey("configure/v1")
	if key == nil {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	if user == "" {
		mac.Write([]byte("configure:" + teamID + ":" + channel + ":" + body))
	} else {
		mac.Write([]byte("configure-user:" + teamID + ":" + channel + ":" + user + ":" + body))
	}
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// mintConfigureToken makes a channel link (user "") or a personal one. Shapes:
//
//	v1.<expiry>.<epoch>.<nonce>.<mac>            — the channel, shared
//	v2.<expiry>.<epoch>.<user>.<nonce>.<mac>     — the channel as seen by one person
func mintConfigureToken(teamID, channel, user string, epoch int64, now time.Time) string {
	nonce := make([]byte, 8)
	rand.Read(nonce)
	body := fmt.Sprintf("v1.%d.%d.%s", now.Add(configureLinkTTL).Unix(), epoch, hex.EncodeToString(nonce))
	if user != "" {
		body = fmt.Sprintf("v2.%d.%d.%s.%s", now.Add(configureLinkTTL).Unix(), epoch, user, hex.EncodeToString(nonce))
	}
	mac := configureMAC(teamID, channel, user, body)
	if mac == "" {
		return ""
	}
	return body + "." + mac
}

// verifyConfigureToken checks the MAC and the expiry and hands back the epoch the link was
// minted under; the caller compares that with the scope's current one.
func verifyConfigureToken(teamID, channel, tok string, now time.Time) (int64, string, bool) {
	i := strings.LastIndex(tok, ".")
	if i < 0 {
		return 0, "", false
	}
	body, sig := tok[:i], tok[i+1:]
	parts := strings.Split(body, ".")
	// Who the link claims to be for is read out of the body first and then checked by the MAC,
	// so rewriting it into somebody else's id invalidates the signature rather than switching
	// whose settings the page opens.
	user := ""
	switch {
	case len(parts) == 4 && parts[0] == "v1":
	case len(parts) == 5 && parts[0] == "v2":
		user = parts[3]
		if user == "" {
			return 0, "", false // a v2 link naming nobody would verify under the channel domain
		}
	default:
		return 0, "", false
	}
	want := configureMAC(teamID, channel, user, body)
	if want == "" || !hmac.Equal([]byte(sig), []byte(want)) {
		return 0, "", false
	}
	exp, err1 := strconv.ParseInt(parts[1], 10, 64)
	epoch, err2 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil || now.Unix() > exp {
		return 0, "", false
	}
	return epoch, user, true
}

var configurePage = template.Must(template.New("configure").Parse(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Configure {{.Bot}} for {{.Scope.Name}}</title>
<style>
:root{color-scheme:light dark;--bg:#fbfaf8;--fg:#191c2b;--card:#fff;--muted:#f2f1ed;--muted-fg:#5d6170;--border:#e8e6e1;--input:#d4d2cb;--primary:#5a50c8;--ok:#2e7d4f;--bad:#bf3b2b;--accent:#eeedf7}
:root{--chev:url("data:image/svg+xml;utf8,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 10 6'><path d='M1 1l4 4 4-4' fill='none' stroke='%235d6170' stroke-width='1.6' stroke-linecap='round' stroke-linejoin='round'/></svg>")}
@media (prefers-color-scheme:dark){:root{--bg:#101019;--fg:#eceaf3;--card:#17161f;--muted:#1d1c27;--muted-fg:#9a97a8;--border:#292834;--input:#363443;--primary:#8a80e8;--ok:#4caf7d;--bad:#e0604f;--accent:#232138}}
@media (prefers-color-scheme:dark){:root{--chev:url("data:image/svg+xml;utf8,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 10 6'><path d='M1 1l4 4 4-4' fill='none' stroke='%239a97a8' stroke-width='1.6' stroke-linecap='round' stroke-linejoin='round'/></svg>")}}
body{margin:0;background:var(--bg);color:var(--fg);font:13px/1.5 Geist,ui-sans-serif,system-ui,sans-serif}
main{max-width:760px;margin:40px auto;padding:0 20px}h1{font-size:20px;margin:0 0 4px}.sub{color:var(--muted-fg);margin:0 0 4px}.id{font-family:ui-monospace,monospace;font-size:11px;color:var(--muted-fg);margin-bottom:18px}
/* Underlined tabs on a single rule, with the reader's own tab pushed to the far right. The
   division is carried by position rather than by two shouty group labels sitting inline, which
   read as tabs themselves and made the bar hard to scan. */
.tabs{display:flex;align-items:stretch;gap:2px;margin-bottom:20px;border-bottom:1px solid var(--border);flex-wrap:wrap}
.tabs a{padding:9px 12px;color:var(--muted-fg);text-decoration:none;border-bottom:2px solid transparent;margin-bottom:-1px;white-space:nowrap}
.tabs a:hover{color:var(--fg)}
.tabs a.on{color:var(--fg);font-weight:550;border-bottom-color:var(--primary)}
.tabs a.mine{margin-left:auto;padding-left:16px;border-left:1px solid var(--border)}
.tabs a.mine:before{content:"";display:inline-block;width:6px;height:6px;border-radius:50%;background:var(--primary);margin-right:7px;vertical-align:1px}
.pconn{border:1px solid var(--border);border-radius:4px;padding:14px;margin-top:12px}
.pconn h3{margin:0;font-size:14px}
.pconn .who{font-size:12px;color:var(--muted-fg);margin:3px 0 12px}
.pconn label{font-size:12px;font-weight:600;margin-bottom:5px}
/* Prose, not a config file: the page-wide monospace box is right for channel instructions and
   wrong for a sentence about how somebody wants their own mail written. */
.pconn textarea{min-height:104px;font:13px/1.55 inherit;font-family:inherit;padding:9px 10px}
.pconn .foot{display:flex;align-items:center;gap:10px;margin-top:8px}
.card{background:var(--card);border:1px solid var(--border);border-radius:6px;box-shadow:0 1px 2px rgba(16,24,40,.04);padding:16px;margin-bottom:12px}
.row{display:flex;justify-content:space-between;gap:16px;align-items:flex-start;padding:10px 0;border-bottom:1px solid var(--border)}.row:last-child{border-bottom:0}
.row .t{font-weight:600}.row .d{color:var(--muted-fg)}label{display:block;font-weight:600;margin:0 0 6px}
textarea{width:100%;box-sizing:border-box;min-height:180px;border:1px solid var(--input);border-radius:3px;background:var(--card);color:var(--fg);padding:8px;font:12px/1.5 ui-monospace,Menlo,monospace}
/* The closed control is ours; the popup a plain <select> opens is drawn by the operating system
   and cannot be styled at all. So: take the native arrow off and draw our own, then hand the
   popup over to CSS as well on browsers that support base-select. Anywhere else the control
   still looks right and the list falls back to the system one, which is the honest trade for a
   page that deliberately runs no JavaScript. */
select{appearance:none;-webkit-appearance:none;border:1px solid var(--input);border-radius:3px;
 background-color:var(--card);color:var(--fg);padding:6px 26px 6px 9px;font:inherit;line-height:1.45;cursor:pointer;
 background-image:var(--chev);background-repeat:no-repeat;background-position:right 8px center;background-size:10px 6px}
select:hover{border-color:var(--muted-fg)}
select:focus-visible{outline:2px solid var(--primary);outline-offset:1px;border-color:var(--primary)}
@supports (appearance:base-select){
  select,::picker(select){appearance:base-select}
  /* base-select draws its own picker icon, so the painted-on chevron has to go or the control
     shows two. */
  select{background-image:none;padding:6px 9px;display:inline-flex;align-items:center;gap:8px;
   justify-content:space-between;min-width:190px}
  select::picker-icon{color:var(--muted-fg);transition:rotate .15s}
  select:open::picker-icon{rotate:180deg}
  ::picker(select){background:var(--card);border:1px solid var(--border);border-radius:6px;padding:4px;
   box-shadow:0 10px 30px rgba(16,24,40,.14);margin-top:4px}
  option{padding:6px 9px;border-radius:3px;color:var(--fg);background:transparent}
  option:hover{background:var(--muted)}
  option:checked{background:var(--accent);font-weight:550}
  option::checkmark{color:var(--primary)}
}
/* The page's one number field. A bare <input> keeps the browser's own chrome — a white box with
   a native bevel, which in the dark theme sits in the card like a hole punched through it — so it
   borrows the select's border, background and metrics and lines up with it in the same row. */
input[type=number]{box-sizing:border-box;flex:none;width:5.5em;border:1px solid var(--input);border-radius:3px;
 background-color:var(--card);color:var(--fg);padding:6px 9px;font:inherit;line-height:1.45;font-variant-numeric:tabular-nums}
input[type=number]:hover{border-color:var(--muted-fg)}
input[type=number]:focus-visible{outline:2px solid var(--primary);outline-offset:1px;border-color:var(--primary)}
/* Read-only viewers get both of these disabled, and a disabled control the browser greys itself
   greys towards white here. Say what off looks like. */
select:disabled,input:disabled,textarea[readonly]{background-color:var(--muted);color:var(--muted-fg);cursor:default}
select:disabled:hover,input:disabled:hover{border-color:var(--input)}
button{background:var(--primary);color:#fff;border:0;border-radius:3px;padding:7px 12px;font:inherit;font-weight:500;cursor:pointer}button.ghost{background:transparent;color:var(--fg);border:1px solid var(--border)}
table{width:100%;border-collapse:collapse}th{font-size:11px;letter-spacing:.05em;text-transform:uppercase;color:var(--muted-fg);text-align:left;padding:6px 8px;border-bottom:1px solid var(--border)}td{padding:8px;border-bottom:1px solid var(--border);vertical-align:top}tr:last-child td{border-bottom:0}
.mono{font-family:ui-monospace,monospace;font-size:12px;color:var(--muted-fg)}.chip{display:inline-block;font-size:11px;background:var(--muted);border:1px solid var(--border);border-radius:3px;padding:2px 6px}
/* Memory tab. The page-wide textarea is a monospace config box 180px tall, which is right for
   channel instructions and wrong for a one-line note about somebody's leave: eight of them filled
   a screen and read like source code. These are prose, sized to their content where the browser
   can do it and two rows where it cannot. */
.subtabs{display:inline-flex;gap:2px;background:var(--muted);border:1px solid var(--border);border-radius:7px;padding:3px;margin-bottom:16px}
.subtabs a{padding:6px 13px;border-radius:5px;color:var(--muted-fg);text-decoration:none;font-weight:500;white-space:nowrap}
.subtabs a:hover{color:var(--fg)}
.subtabs a.on{background:var(--card);color:var(--fg);box-shadow:0 1px 2px rgba(16,24,40,.06)}
.subtabs .n{font-size:11px;color:var(--muted-fg);margin-left:5px}
.mem{padding:12px 0;border-bottom:1px solid var(--border)}
.mem:last-of-type{border-bottom:0;padding-bottom:0}
.mem-head{display:flex;align-items:center;gap:8px;margin-bottom:7px}
.mem-head time{font-size:11px;color:var(--muted-fg);margin-left:auto;font-variant-numeric:tabular-nums}
.mem textarea{min-height:0;height:auto;field-sizing:content;font:13px/1.55 inherit;font-family:inherit;padding:9px 11px}
/* Always shown, never on hover. Deletion used to mean emptying the box and saving, which
   nobody found; hiding the button that replaced it until the pointer lands would be the same
   mistake wearing a nicer coat. */
.mem-act{display:flex;gap:8px;justify-content:flex-end;margin-top:8px}
.mem-act button{padding:5px 11px;font-size:12px}
button.danger:hover{color:var(--bad);border-color:var(--bad)}
.ok{color:var(--ok)}.bad{color:var(--bad)}.note{color:var(--muted-fg);font-size:12px;margin-top:8px}.empty{text-align:center;color:var(--muted-fg);padding:28px 0}
.saved{color:var(--ok);font-weight:500}
</style>
<main>
<h1>Configure {{.Bot}} for {{.Scope.Name}}</h1>
<p class="sub">{{if .ReadOnly}}Only admins can change how {{.Bot}} behaves here.{{else}}Anyone in this Slack channel can adjust how {{.Bot}} behaves here.{{end}}</p>
<div class="id">{{.Scope.SlackID}}</div>
<div class="tabs"><a class="{{if eq .Tab "general"}}on{{end}}" href="?t={{.Token}}&tab=general">General</a><a class="{{if eq .Tab "tools"}}on{{end}}" href="?t={{.Token}}&tab=tools">Tools and access</a><a class="{{if eq .Tab "memory"}}on{{end}}" href="?t={{.Token}}&tab=memory">Memory</a><a class="{{if eq .Tab "routines"}}on{{end}}" href="?t={{.Token}}&tab=routines">Routines</a></div>
{{if .Saved}}<p class="saved">Saved.</p>{{end}}
{{if eq .Tab "general"}}
<form method="post" class="card">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<input type="hidden" name="tab" value="general">
<div class="row"><div><div class="t">Read every message</div><div class="d">When off, {{.Bot}} replies here only when @-mentioned. When on, it reads every message here and decides for itself whether to answer, react with an emoji, or say nothing — judged against the channel instructions below. "Inherit" follows the workspace.</div></div>
<select name="read_all" {{if .ReadOnly}}disabled{{end}}><option value="inherit" {{if eq .Scope.ReadAll "inherit"}}selected{{end}}>Inherit</option><option value="on" {{if eq .Scope.ReadAll "on"}}selected{{end}}>On</option><option value="off" {{if eq .Scope.ReadAll "off"}}selected{{end}}>Off</option></select></div>
<div class="row"><div><div class="t">Email intake</div><div class="d">Whether mail forwarded to this channel's Slack email address is answered. {{if eq .Scope.EmailIntake "off"}}Off here.{{else if .EmailIntakeOn}}<b>On.</b> A mail forwarded here is answered as a turn nobody in the workspace started: it runs on this channel's own connections, never on anyone's personal account, and {{if .EmailAutoWritesOn}}a write covered by one of the pre-approved actions below runs straight away — everything else waits for a named approver{{else}}every write it proposes waits for a named approver{{end}}. Anyone here can stop it; only an admin can start it again, in the console.{{else}}Off — nothing happens when mail arrives here.{{end}}</div></div>
{{if .EmailIntakeOn}}<button type="submit" name="email_intake" value="off" {{if .ReadOnly}}disabled{{end}}>Turn off here</button>{{else}}<span class="note">Set by your admins.</span>{{end}}</div>
<div class="row"><div><div class="t">Model</div><div class="d">Which model answers in this channel. "Default" follows Settings; "Advanced" uses the stronger model for every turn.{{if .ChannelModels}} An admin has also offered the models listed below.{{end}}</div></div>
<select name="default_model" {{if .ReadOnly}}disabled{{end}}><option value="" {{if eq .Scope.DefaultModel ""}}selected{{end}}>Default ({{.DefaultModel}})</option>{{if .HeavyModel}}<option value="heavy" {{if eq .Scope.DefaultModel "heavy"}}selected{{end}}>Advanced ({{.HeavyModel}})</option>{{end}}{{range .ChannelModels}}<option value="{{.}}" {{if eq $.Scope.DefaultModel .}}selected{{end}}>{{.}}</option>{{end}}{{if .OtherModel}}<option value="{{.OtherModel}}" selected>{{.OtherModel}} (set in the console)</option>{{end}}</select></div>
<div class="row"><div><div class="t">Tool rounds per reply</div><div class="d">How many rounds of tool calls one reply here may spend before it has to answer with what it has. A channel where {{.Bot}} reads logs or traces an alert needs more of them than a conversation does; a reply that runs out now answers with what it found rather than giving up. 0 follows the workspace, and {{.MaxRounds}} is the most any reply will take.</div></div>
<input type="number" name="max_tool_rounds" min="0" max="{{.MaxRounds}}" value="{{.Scope.MaxToolRounds}}" {{if .ReadOnly}}disabled{{end}}></div>
<div class="row" style="display:block"><label>Channel instructions</label>
<textarea name="instructions" {{if .ReadOnly}}readonly{{end}}>{{.Scope.Instructions}}</textarea>
<div class="note">Added to {{.Bot}}'s instructions for every new session in this channel, alongside any instructions set by your admins.</div></div>
{{if not .ReadOnly}}<div style="text-align:right;margin-top:10px"><button type="submit">Save</button></div>{{end}}
</form>
{{else if eq .Tab "tools"}}
{{if or .HasPersonal .SubMine}}<div class="subtabs"><a class="{{if not .SubMine}}on{{end}}" href="?t={{.Token}}&tab=tools">This channel</a><a class="mine {{if .SubMine}}on{{end}}" href="?t={{.Token}}&tab=tools&sub=mine">Your accounts</a></div>{{end}}
{{if .SubMine}}
{{if not .AsUser}}
<div class="card">
  <div class="note">These run on <strong>your own</strong> account, not the channel's — so this page has to know who you are before it can show them, and the Configure link under {{.Bot}}'s replies does not: everyone in {{.Scope.Name}} shares it.</div>
  <div class="empty" style="padding:20px 0 8px">Say <span class="mono">!personal_instructions</span> to {{.Bot}} in Slack.<br>It sends you a private link to this page that opens your own settings.</div>
</div>
{{else}}
<div class="card"><div class="note">These run on <strong>your own</strong> account, not the channel's. What you write here reaches {{.Bot}} only on your own turns — nobody else in {{.Scope.Name}} sees it or is affected by it.</div>
{{range .Personal}}<div class="pconn">
  <h3>{{.Name}}</h3>
  <p class="who">{{if .Connected}}Connected{{if .Account}} as {{.Account}}{{end}}.{{else}}Not connected yet — say <span class="mono">!connect</span> in Slack. You can still write instructions now; {{$.Bot}} will follow them once you connect.{{end}}</p>
  <form method="post">
    <input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="tab" value="personal">
    <input type="hidden" name="conn" value="{{.ConnID}}">
    <label for="ins{{.ConnID}}">How should {{$.Bot}} use it?</label>
    <textarea id="ins{{.ConnID}}" name="instructions" rows="5" placeholder="{{.Hint}}">{{.Instructions}}</textarea>
    <div class="foot"><button type="submit">Save</button>
      <span class="note" style="margin:0">How to go about things, as much as how to sound.</span></div>
  </form>
</div>{{end}}</div>
{{end}}
{{else}}
<div class="card"><div class="t" style="font-weight:600">Tool packs</div><div class="note">Named tools available to {{.Bot}} in this channel, from the attached bundles.</div>
{{if .Packs}}<p>{{range .Packs}}<span class="chip">{{.}}</span> {{end}}</p>{{else}}<p class="note">No tool packs enabled.</p>{{end}}</div>
<div class="card"><div class="t" style="font-weight:600">Connections</div><div class="note">Services {{.Bot}} can reach for sessions in this channel, named bundle/connection. Set by your admins, through a bundle or one connection at a time.</div>
{{if .Access}}<table><tr><th>Name</th><th>Access</th><th>From</th></tr>{{range .Access}}<tr><td>{{.Name}}</td><td class="mono">{{.Hosts}}</td><td class="note">{{.Origin}}</td></tr>{{end}}</table>{{else}}<div class="empty">No connections configured.</div>{{end}}</div>
<div class="card"><div class="t" style="font-weight:600">Pre-approved actions</div><div class="note">Writes {{.Bot}} may do here without someone pressing Confirm first. Set by your admins.</div>
{{if .AllowRules}}<ul>{{range .AllowRules}}<li>{{.}}</li>{{end}}</ul>
<p class="note">{{if .EmailAutoWritesOn}}These also apply to mail forwarded here, judged on where the write goes rather than on anything the mail says. Opening a pull request never is: that always waits for a named approver.{{else}}These do not apply to mail forwarded here — a turn nobody in the workspace started holds every write.{{end}}</p>{{else}}<p class="note">None. Every write waits for someone in the thread to press Confirm; reading is never held.</p>{{end}}</div>
{{end}}
{{else if eq .Tab "memory"}}
<div class="subtabs"><a class="{{if not .SubMine}}on{{end}}" href="?t={{.Token}}&tab=memory">This channel</a><a class="mine {{if .SubMine}}on{{end}}" href="?t={{.Token}}&tab=memory&sub=mine">Yours{{if and .AsUser .MyNotes}}<span class="n">{{len .MyNotes}}</span>{{end}}</a></div>
{{if .SubMine}}
{{if .AsUser}}
<div class="card"><div class="t" style="font-weight:600">Your own notes</div>
<div class="note">Private to you. Nobody else in {{.Scope.Name}} can see these — not your teammates, not your admins — and {{.Bot}} only reads them on your own turns.</div>
{{if .MyNotes}}{{range .MyNotes}}<form method="post" class="mem">
  <input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="tab" value="memory">
  <input type="hidden" name="sub" value="mine"><input type="hidden" name="kind" value="personal">
  <input type="hidden" name="memory" value="{{.ID}}">
  <div class="mem-head"><span class="chip">only you</span><time>{{.At}}</time></div>
  <textarea name="text" rows="2" aria-label="Note">{{.Text}}</textarea>
  <div class="mem-act"><button class="ghost danger" type="submit" name="act" value="delete">Delete</button><button type="submit" name="act" value="save">Save</button></div>
</form>{{end}}{{else}}<div class="empty">No private notes yet.<br><span class="note">Say <span class="mono">remember for me: …</span> to {{.Bot}} in Slack, or <span class="mono">!note …</span></span></div>{{end}}
<div class="note">Deleting a note removes it from what {{.Bot}} reads. Anything it already said in a conversation stays in that conversation.</div></div>
{{else}}
<div class="card"><div class="t" style="font-weight:600">Your own notes</div>
<div class="note">Notes {{.Bot}} reads only for you, in any channel. This page cannot show them: the Configure link under {{.Bot}}'s replies is shared by everyone in {{.Scope.Name}}, and these are not.</div>
<div class="empty" style="padding:20px 0 8px">Say <span class="mono">!notes</span> to {{.Bot}} in Slack.<br>It sends you a private link that opens your own.</div></div>
{{end}}
{{else}}
<div class="card"><div class="t" style="font-weight:600">What {{.Bot}} remembers here</div>
<div class="note">Facts someone asked it to keep, shared with everyone in {{.Scope.Name}}. A memory marked <em>this workspace</em> is read in every public channel, not just this one.</div>
{{if .Memories}}{{range .Memories}}<form method="post" class="mem">
  <input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="tab" value="memory">
  <input type="hidden" name="kind" value="shared"><input type="hidden" name="memory" value="{{.ID}}">
  <div class="mem-head"><span class="chip">{{.Where}}</span><time>{{.At}}</time></div>
  <textarea name="text" rows="2" aria-label="Memory" {{if $.ReadOnly}}readonly{{end}}>{{.Text}}</textarea>
  {{if not $.ReadOnly}}<div class="mem-act"><button class="ghost danger" type="submit" name="act" value="delete">Delete</button><button type="submit" name="act" value="save">Save</button></div>{{end}}
</form>{{end}}{{else}}<div class="empty">Nothing remembered yet.<br><span class="note">Say <span class="mono">remember for this channel: …</span> to {{.Bot}} in Slack.</span></div>{{end}}</div>
{{end}}
{{else}}
<div class="card"><div class="note">Scheduled work {{.Bot}} runs in this channel. Manage routines from Slack with @{{.Bot}} !routines or by asking {{.Bot}} to schedule something.</div>
<div class="note">Each run may spend <strong>{{.RoutineRounds}} rounds of tool calls</strong> in <strong>{{.RoutineMinutes}} minutes</strong>, the write-up included. That is its own budget, not the channel's tool rounds per reply: nobody is waiting on a scheduled run, so it is allowed to do more than an answer in a thread. An admin changes it in the console under Settings.</div>
{{if .Routines}}<table><tr><th>Prompt</th><th>Schedule</th><th>Status</th><th>Last run</th>{{if not .ReadOnly}}<th></th>{{end}}</tr>
{{range .Routines}}<tr><td>{{.Prompt}}{{if .AutoConfirm}}<div class="note">Writes run without asking.</div>{{end}}</td><td class="mono">{{.Cron}} {{.TZ}}</td><td>{{if .Enabled}}<span class="ok">Enabled</span>{{else}}<span class="note">Disabled</span>{{end}}{{if .LastError}}<div class="bad">{{.LastError}}</div>{{end}}</td><td class="note">{{.LastRun}}</td>
{{if not $.ReadOnly}}<td><form method="post" style="margin:0"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="tab" value="routines"><input type="hidden" name="routine" value="{{.ID}}"><input type="hidden" name="enabled" value="{{if .Enabled}}0{{else}}1{{end}}"><button class="ghost" type="submit">{{if .Enabled}}Disable{{else}}Enable{{end}}</button></form></td>{{end}}</tr>{{end}}</table>
{{else}}<div class="empty">No routines are set up for this channel yet.</div>{{end}}</div>
{{end}}
</main>`))

type accessRow struct{ Name, Hosts, Origin string }

// personalRow is one connection that runs on the reader's own account, with what they have told
// the bot about using it. Nobody else's row is ever built: the person comes from the link's MAC.
type personalRow struct {
	ConnID       int64
	Name         string
	Account      string
	Connected    bool
	Instructions string
	Hint         string
}

// personalConnection is the guard on the save: the connection has to be one this channel can
// actually reach and one that runs on each person's own account. Without it the form's `conn`
// field would name any connection in the database.
func (b *Bot) personalConnection(ctx context.Context, orgID int64, teamID, channel string, connID int64) *Connection {
	if connID == 0 {
		return nil
	}
	acc, err := b.resolver.Resolve(ctx, orgID, teamID, channel, b.settings.Get(ctx, orgID).ConfigVersion)
	if err != nil || acc == nil {
		return nil
	}
	for _, rl := range acc.Rules {
		if rl.Conn != nil && rl.Conn.ID == connID && rl.Conn.CredType == "oauth_user" {
			return rl.Conn
		}
	}
	return nil
}

// configureCSRFCookie backs the page's forms. The link is the authority to read the page, but a
// POST also has to come from the page itself: with only the link, any site the reader visited
// could submit the form for them once the URL was known.
const configureCSRFCookie = "attest_cfg_csrf"

const configureInstructionsMax = 4000

func (b *Bot) handleConfigure(w http.ResponseWriter, r *http.Request) {
	teamID, channel := r.PathValue("team"), r.PathValue("channel")
	epoch, asUser, ok := verifyConfigureToken(teamID, channel, r.URL.Query().Get("t"), time.Now())
	if !ok {
		http.Error(w, "This link has expired or is not valid. Open Configure from one of the bot's newer replies in the channel.", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	csrf := ""
	if c, err := r.Cookie(configureCSRFCookie); err == nil {
		csrf = c.Value
	}
	if r.Method == http.MethodPost {
		if csrf == "" || !hmac.Equal([]byte(r.FormValue("csrf")), []byte(csrf)) {
			http.Error(w, "This form has expired. Reload the page and try again.", http.StatusForbidden)
			return
		}
	} else if csrf == "" {
		csrf = randomToken()
		http.SetCookie(w, &http.Cookie{Name: configureCSRFCookie, Value: csrf, Path: "/configure/", HttpOnly: true,
			MaxAge: 3600, SameSite: http.SameSiteStrictMode, Secure: b.secureCookies(r)})
	}
	// No session here — the link is the authority — so the organisation comes from the
	// workspace the link names, which the token already covers.
	orgID, err := b.store.OrgOfTeam(ctx, teamID)
	if err != nil {
		http.Error(w, "That workspace is not connected any more.", http.StatusNotFound)
		return
	}
	sc, _ := b.store.ChannelScope(ctx, orgID, teamID, channel)
	if sc == nil {
		b.syncScopesFor(ctx, orgID, teamID)
		if sc, _ = b.store.ChannelScope(ctx, orgID, teamID, channel); sc == nil {
			sc, _ = b.store.UpsertChannelScope(ctx, orgID, teamID, channel,
				"#"+b.channelName(ctx, teamID, channel), b.channelPrivate(ctx, teamID, channel))
		}
	}
	if sc == nil {
		http.Error(w, "That channel is not in a connected Slack workspace.", http.StatusNotFound)
		return
	}
	// A link minted before an admin revoked this channel's links is void, however fresh.
	if epoch != sc.LinkEpoch {
		http.Error(w, "This link was revoked. Open Configure from one of the bot's newer replies in the channel.", http.StatusForbidden)
		return
	}
	readOnly := b.memberEditsBlocked(ctx, orgID, sc)
	tab := r.URL.Query().Get("tab")
	if tab == "" {
		tab = "general"
	}
	saved := false
	if r.Method == http.MethodPost && !readOnly {
		r.ParseForm()
		tab = r.FormValue("tab")
		switch tab {
		case "general":
			// "" is the workspace default, "heavy" is the stronger model, and anything else has
			// to be one an admin offered. A channel page is open to everyone who can see the
			// channel, so a model name typed into the form is not a name anyone chose to pay for.
			dm := r.FormValue("default_model")
			if dm != "" && dm != "heavy" && !slices.Contains(b.settings.Get(ctx, orgID).ChannelModels, dm) {
				dm = sc.DefaultModel
			}
			// Instructions go straight into the model's system prompt for this channel; a bound
			// keeps one page from swallowing the context window.
			instructions := r.FormValue("instructions")
			if len(instructions) > configureInstructionsMax {
				http.Error(w, fmt.Sprintf("Channel instructions are limited to %d characters.", configureInstructionsMax), http.StatusBadRequest)
				return
			}
			b.store.UpdateScope(ctx, orgID, sc.ID, instructions, dm, sc.MemberEdits)
			b.store.SetScopeReadAll(ctx, orgID, sc.ID, r.FormValue("read_all"))
			// Only ever "off". Turning email intake on means answering mail from outside the
			// company, and this link is shared under every reply — anyone who can see the channel
			// can open it. Stopping something is not the same permission as starting it.
			if r.FormValue("email_intake") == "off" {
				b.store.SetScopeEmailIntake(ctx, orgID, sc.ID, "off")
			}
			// An empty or unreadable box is "inherit", the same as zero; the store clamps the
			// rest, so nothing typed here can ask for more rounds than a turn will run.
			rounds, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("max_tool_rounds")))
			b.store.SetScopeMaxToolRounds(ctx, orgID, sc.ID, rounds)
			b.changed(ctx, orgID)
			saved = true
		case "personal":
			// Only ever this person's own row, because the id comes from the MAC over the link
			// rather than from anything the form could carry.
			if asUser != "" {
				connID, _ := strconv.ParseInt(r.FormValue("conn"), 10, 64)
				if b.personalConnection(ctx, orgID, teamID, channel, connID) != nil {
					if err := b.store.SaveUserInstructions(ctx, orgID, connID, teamID, asUser,
						strings.TrimSpace(r.FormValue("instructions"))); err == nil {
						saved = true
					}
				}
			}
		case "memory":
			// Two kinds of row on one tab, and which one a request may touch is decided by the
			// link it arrived on, not by the form. A shared link edits what the channel and the
			// workspace remember; only a link that names somebody can touch that person's own
			// notes, and the name comes from the MAC over the link.
			id, _ := strconv.ParseInt(r.FormValue("memory"), 10, 64)
			// Delete is its own button rather than "empty the box and save", which nobody found:
			// the only way to remove a memory was to discover that a blank box meant delete.
			gone := r.FormValue("act") == "delete"
			switch {
			case r.FormValue("kind") == "personal" && asUser != "":
				k := personalKey{orgID: orgID, teamID: teamID, owner: asUser}
				text := strings.TrimSpace(r.FormValue("text"))
				if gone || text == "" {
					ok, err := b.store.DeletePersonalMemory(ctx, k, id)
					saved = err == nil && ok
				} else if ok, err := b.store.UpdatePersonalMemory(ctx, k, id, text); err == nil {
					saved = ok
				}
			case r.FormValue("kind") == "shared":
				// Scoped to this organisation, and then checked against the scopes this channel
				// actually reads, so an id from another channel is not editable from this page.
				text := strings.TrimSpace(r.FormValue("text"))
				if b.channelMemory(ctx, orgID, teamID, channel, id) != nil {
					if gone || text == "" {
						saved = b.store.DeleteMemory(ctx, orgID, id) == nil
					} else if ok, err := b.store.UpdateMemory(ctx, orgID, id, text); err == nil {
						saved = ok
					}
				}
			}
		case "routines":
			id, _ := strconv.ParseInt(r.FormValue("routine"), 10, 64)
			if rs, _ := b.store.Routines(ctx, orgID, channel); rs != nil {
				for _, rt := range rs {
					if rt.ID == id {
						b.store.SetRoutineEnabled(ctx, orgID, id, r.FormValue("enabled") == "1")
						saved = true
					}
				}
			}
		}
		sc, _ = b.store.ChannelScope(ctx, orgID, teamID, channel)
	}
	// Which side of a tab: the channel's, or the reader's own. It is only the requested view --
	// what that view may *show* is gated on asUser inside the template, so a hand-typed &sub=mine
	// on a shared link lands on a page that explains how to get a link of your own rather than on
	// somebody's notes. tab=personal still works because every link !personal_instructions ever
	// DMed carries it, and those outlive this rename.
	subMine := r.URL.Query().Get("sub") == "mine" || r.FormValue("sub") == "mine" || tab == "personal"
	if tab == "personal" {
		tab = "tools"
	}
	acc, _ := b.resolver.Resolve(ctx, orgID, teamID, channel, b.settings.Get(ctx, orgID).ConfigVersion)
	var rows []accessRow
	var packs []string
	if acc != nil {
		names := map[int64]string{}
		if bs, _ := b.store.Bundles(ctx, orgID); bs != nil {
			for _, bd := range bs {
				names[bd.ID] = bd.Name
			}
		}
		for _, rl := range acc.Rules {
			origin := "workspace"
			if rl.Rank == 2 {
				origin = "this channel"
			}
			from := "bundle " + names[rl.Conn.BundleID] + " · " + origin
			if rl.Direct {
				from = "on its own · " + origin
			}
			rows = append(rows, accessRow{Name: names[rl.Conn.BundleID] + "/" + rl.Conn.Name, Hosts: strings.Join(rl.Conn.AllowedHosts, ", "), Origin: from})
		}
		for _, d := range acc.Domains {
			rows = append(rows, accessRow{Name: "(no credential)", Hosts: d.Host, Origin: names[d.BundleID]})
		}
		for p := range acc.ToolPacks {
			if pr := presetByID(p); pr != nil {
				packs = append(packs, pr.Name)
			}
		}
		sortStrings(packs)
	}
	// The Personal tab shows wherever this channel has a connection that runs on each person's
	// own account. Its *contents* need a link that names somebody, and the footer link under
	// every reply names nobody — it is read by everyone who can see the channel. So on a shared
	// link the tab is still drawn and says how to get a link of your own: leaving it out
	// altogether meant nobody ever discovered the tab existed, since the footer link is the only
	// one most people ever hold.
	var personal []personalRow
	hasPersonal := false
	if acc != nil {
		for _, rl := range acc.Rules {
			if rl.Conn == nil || rl.Conn.CredType != "oauth_user" {
				continue
			}
			hasPersonal = true
			if asUser == "" {
				continue // a shared link: name the tab, show nobody's settings in it
			}
			row := personalRow{ConnID: rl.Conn.ID, Name: rl.Conn.Name, Hint: instructionsHint(rl.Conn)}
			if uc, _ := b.store.UserConnection(ctx, orgID, rl.Conn.ID, teamID, asUser); uc != nil {
				row.Instructions, row.Account, row.Connected = uc.Instructions, uc.Account, uc.Connected()
			}
			personal = append(personal, row)
		}
	}
	// The Memory tab, which is two lists on one page because they answer the same question at
	// two ranges: what this room knows, and what you asked to be kept for you. The second is only
	// drawn on a link that names somebody -- a shared link cannot show it, because it is read by
	// everyone who can see the channel.
	shared, _ := b.store.Memories(ctx, orgID, channelMemoryScope(teamID, channel), teamMemoryScope(teamID))
	var sharedRows []memoryRow
	for _, m := range shared {
		where := "this workspace"
		if strings.HasPrefix(m.Scope, "channel:") {
			where = "this channel"
		}
		sharedRows = append(sharedRows, memoryRow{ID: m.ID, Text: m.Text, Where: where, At: m.At})
	}
	var mine []memoryRow
	if asUser != "" {
		notes, _ := b.store.PersonalMemories(ctx, personalKey{orgID: orgID, teamID: teamID, owner: asUser})
		for _, n := range notes {
			mine = append(mine, memoryRow{ID: n.ID, Text: n.Text, Where: "only you", At: n.CreatedAt})
		}
	}
	routines, _ := b.store.Routines(ctx, orgID, channel)
	allow := append([]string{}, b.settings.Get(ctx, orgID).AllowRules...)
	if acc != nil {
		allow = append(allow, acc.AllowRules...)
	}
	routineRounds, routineWall := routineBudget(b.settings.Get(ctx, orgID))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	configurePage.Execute(w, map[string]any{
		"Bot": b.settings.Get(ctx, orgID).BotName, "Scope": sc, "Token": mintConfigureToken(teamID, channel, asUser, sc.LinkEpoch, time.Now()), "CSRF": csrf, "Tab": tab, "Saved": saved,
		"ReadOnly": readOnly, "Access": rows, "Packs": packs, "Routines": routines, "AllowRules": allow,
		"Personal": personal, "AsUser": asUser, "HasPersonal": hasPersonal,
		"Memories": sharedRows, "MyNotes": mine, "SubMine": subMine,
		"DefaultModel": inheritedDefault(ctx, b.store, b.settings.Get(ctx, orgID), orgID, sc.TeamID), "HeavyModel": b.settings.Get(ctx, orgID).HeavyModel,
		"ChannelModels": b.settings.Get(ctx, orgID).ChannelModels, "OtherModel": otherModel(sc.DefaultModel, b.settings.Get(ctx, orgID).ChannelModels),
		"MaxRounds": maxRoundsCeiling, "RoutineRounds": routineRounds, "RoutineMinutes": int(routineWall / time.Minute),
		// Resolved rather than read off this scope: a channel inheriting "on" from its workspace
		// is taking mail, and the row has to say so to the people in it.
		"EmailIntakeOn": b.emailIntakeOn(ctx, orgID, teamID, channel),
		// Read off the resolved Access rather than this scope's own row, for the reason the line
		// above is resolved: a channel inheriting "on" from its workspace is on.
		"EmailAutoWritesOn": acc != nil && acc.EmailAutoWrites,
	})
}

// memoryRow is one line on the Memory tab, shared or personal.
type memoryRow struct {
	ID    int64
	Text  string
	Where string
	At    string
}

// channelMemory finds one of the memories this channel actually reads -- its own or its
// workspace's -- so an id typed into the form cannot reach a memory belonging to a different
// channel of the same organisation. The organisation's own console can edit any of them; this
// page is open to everyone who can see one channel, and should only edit what that channel says.
func (b *Bot) channelMemory(ctx context.Context, orgID int64, teamID, channel string, id int64) *Memory {
	if id == 0 {
		return nil
	}
	mems, err := b.store.Memories(ctx, orgID, channelMemoryScope(teamID, channel), teamMemoryScope(teamID))
	if err != nil {
		return nil
	}
	for i := range mems {
		if mems[i].ID == id {
			return &mems[i]
		}
	}
	return nil
}

// memberEditsBlocked walks the scope chain: a channel inheriting from a blocked workspace is blocked.
func (b *Bot) memberEditsBlocked(ctx context.Context, orgID int64, sc *Scope) bool {
	switch sc.MemberEdits {
	case "block":
		return true
	case "allow":
		return false
	}
	// A channel inherits from its own workspace, and the workspace from the account.
	if team, _ := b.store.TeamScope(ctx, orgID, sc.TeamID); team != nil {
		switch team.MemberEdits {
		case "block":
			return true
		case "allow":
			return false
		}
	}
	if acct, _ := b.store.AccountScope(ctx, orgID); acct != nil && acct.MemberEdits == "block" {
		return true
	}
	return false
}

func (b *Bot) configureRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /configure/{team}/{channel}", b.handleConfigure)
	mux.HandleFunc("POST /configure/{team}/{channel}", b.handleConfigure)
}

var _ = context.Background

// otherModel is a model this channel is already on that is not on the offered list — set from
// the console, or offered once and withdrawn since. It is shown as a selected option so the
// dropdown tells the truth about what is running, and saving any other choice drops it.
func otherModel(current string, offered []string) string {
	if current == "" || current == "heavy" || slices.Contains(offered, current) {
		return ""
	}
	return current
}

// postMemoryLink follows an answer that changed what is remembered with the page that edits it,
// so somebody can fix a memory without a console account. Two links, because they have two
// audiences, and the difference is the whole of the security here.
//
// The shared one goes in the thread: it names a channel and nobody in particular, which is the
// same link already printed under every reply, read by the same people. The personal one goes by
// DM and never anywhere else -- it carries the authority to read and rewrite one person's own
// notes, and everyone who can read a reply can read a link inside it.
//
// Neither is ever returned from a tool. A tool result is prompt text, and prompt text travels to
// whoever serves the model; askToConnect keeps a capability link out of one for that reason, and
// it also means the model cannot mangle, truncate or repeat a link it never saw.
func (a *Agent) postMemoryLink(ctx context.Context, c *Call) {
	if c.SL == nil || c.offline() || a.store == nil {
		return
	}
	if c.personalMemoryTouched {
		if k, ok := c.personalKey(); ok {
			if link := a.personalMemoryURL(ctx, c.OrgID, k.teamID, c.Channel, k.owner); link != "" {
				says := "I saved that for you. This opens the notes I keep for you, to edit or delete any of them."
				if c.Kind == "dm" {
					// Already the one place only they can read, and the place they just asked in.
					// The card goes in the thread they asked in rather than to the top of the DM,
					// where it arrives detached from the thing it is about.
					a.threadCard(ctx, c, "Your notes", says, link)
				} else if a.dmLink(ctx, c, "Your notes", says, link) {
					// The link itself must not be posted here -- everyone who can read this reply
					// could use it. But saying nothing left people with no way to know an edit
					// surface exists at all, so the thread gets the signpost and not the key.
					c.SL.PostText(ctx, c.Channel, c.ThreadTS,
						"I've sent you a private link to edit that note — it's in our DM, since only you can see these.")
				}
			}
		}
	}
	if !c.memoryTouched || c.AnswerTS == "" {
		return
	}
	if link := a.configureURL(ctx, c.OrgID, c.TeamID, c.Channel); link != "" {
		c.SL.PostText(ctx, c.Channel, c.ThreadTS, "<"+link+"&tab=memory|Edit what I remember here> — no sign-in needed.")
	}
}

// threadCard is dmLink's card posted into the thread in hand rather than to a DM, for the one
// case where those are the same audience: a direct message. It keeps the link beside the answer
// it belongs to instead of at the top of the conversation.
func (a *Agent) threadCard(ctx context.Context, c *Call, title, says, link string) bool {
	if c.SL == nil {
		return false
	}
	_, _, err := c.SL.PostCard(ctx, c.Channel, c.ThreadTS, linkCard(title, says, link), title)
	return err == nil
}

// personalMemoryURL is the Memory tab opened as one person, which is what draws their own notes
// on it. Same shape as personalConfigureURL and the same rule: never posted in a channel.
func (a *Agent) personalMemoryURL(ctx context.Context, orgID int64, teamID, channel, slackUserID string) string {
	u := a.personalConfigureURL(ctx, orgID, teamID, channel, slackUserID)
	if u == "" {
		return ""
	}
	return strings.TrimSuffix(u, "&tab=personal") + "&tab=memory"
}
