import html, os, pathlib

# The folder shot.sh reads the HTML from, so the two must agree: $D itself when it is set,
# $TMPDIR/attesttag-mkt otherwise.
OUT = pathlib.Path(os.environ.get("D") or pathlib.Path(os.environ.get("TMPDIR") or "/tmp") / "attesttag-mkt")
MARK = ('<svg viewBox="0 0 256 256" width="30" height="30"><g fill="none" stroke-width="30" stroke-linecap="butt" '
        'stroke-linejoin="round"><path d="M97.6 121.6 L140 172.2 L210.9 112.7" stroke="#5a50c8"/>'
        '<path d="M210.9 112.7 A48 48 0 0 0 180 28 L76 28 A48 48 0 0 0 28 76 L28 180 A48 48 0 0 0 76 228 '
        'L180 228 A48 48 0 0 0 223.5 200.3" stroke="#191c2b"/></g></svg>')

CSS = """
*{box-sizing:border-box;margin:0;padding:0}
body{width:1600px;height:1000px;overflow:hidden;
  font-family:-apple-system,"SF Pro Text","Helvetica Neue",Arial,sans-serif;
  color:#191c2b;background:#f2f1fa;
  background-image:radial-gradient(900px 520px at 88% -12%,#ded9fb 0%,rgba(222,217,251,0) 62%),
                   radial-gradient(700px 460px at -8% 106%,#e2effb 0%,rgba(226,239,251,0) 60%);}
.wrap{padding:64px 72px 0;height:1000px;display:flex;flex-direction:column}
.top{display:flex;align-items:flex-start;justify-content:space-between;gap:40px}
h1{font-size:50px;line-height:1.08;letter-spacing:-.022em;font-weight:700;max-width:1020px}
h1 em{font-style:normal;color:#5a50c8}
.sub{margin-top:16px;font-size:23px;line-height:1.42;color:#585c70;max-width:930px;font-weight:400}
.lock{display:flex;align-items:center;gap:11px;flex:none;padding-top:6px}
.lock .m{width:46px;height:46px;border-radius:12px;background:#fff;display:flex;align-items:center;
  justify-content:center;box-shadow:0 2px 10px rgba(25,28,43,.10)}
.lock .m svg{width:29px;height:29px}
.lock .n{font-size:19px;font-weight:700;letter-spacing:-.01em}
.card{margin-top:36px;flex:1;background:#fff;border-radius:18px;border:1px solid #e4e3ef;
  box-shadow:0 26px 64px rgba(25,28,43,.15);overflow:hidden;display:flex;flex-direction:column}
.chan{height:70px;flex:none;display:flex;align-items:center;gap:12px;padding:0 28px;
  border-bottom:1px solid #ededf2;background:#fcfcfe}
.chan .h{font-size:21px;font-weight:700;letter-spacing:-.01em}
.chan .meta{font-size:15.5px;color:#8a8d9b}
.chan .dot{width:5px;height:5px;border-radius:50%;background:#c9cad6}
.msgs{padding:6px 0 0}
.msg{display:flex;gap:16px;padding:18px 30px 16px}
.msg+.msg{padding-top:12px}
.av{width:48px;height:48px;border-radius:11px;flex:none;display:flex;align-items:center;justify-content:center;
  color:#fff;font-size:17.5px;font-weight:700;letter-spacing:.01em}
.av.app{background:#fff;border:1px solid #e4e3ef}
.av.app svg{width:31px;height:31px}
.hd{display:flex;align-items:baseline;gap:9px;margin-bottom:4px}
.nm{font-size:18.5px;font-weight:700;letter-spacing:-.01em}
.badge{font-size:11.5px;font-weight:700;letter-spacing:.05em;background:#e9e8f0;color:#5f6277;
  border-radius:3px;padding:2px 5px;text-transform:uppercase}
.tm{font-size:14.5px;color:#8a8d9b}
.bd{font-size:19.5px;line-height:1.6;color:#1d1c1d;max-width:1180px}
.bd b{font-weight:700}
.mention{color:#1264a3;background:#e8f5fa;border-radius:3px;padding:0 3px}
.li{display:flex;gap:11px;margin-top:8px}
.li .b{color:#5a50c8;font-weight:700}
code{font-family:"SF Mono",ui-monospace,Menlo,monospace;font-size:17px;background:#f3f2f8;
  border:1px solid #e7e6f0;border-radius:4px;padding:1px 5px;color:#3f3a80}
.status{display:flex;align-items:center;gap:10px;font-size:16.5px;color:#6b6e80;margin-bottom:9px}
.status .p{width:8px;height:8px;border-radius:50%;background:#5a50c8;box-shadow:0 0 0 4px rgba(90,80,200,.16)}
.foot{margin-top:14px;font-size:14.5px;color:#9093a2}
.foot a{color:#5a50c8;text-decoration:none;font-weight:600}
.src{display:flex;gap:9px;margin-top:14px;flex-wrap:wrap}
.chip{display:flex;align-items:center;gap:8px;border:1px solid #e4e3ef;background:#fafaff;
  border-radius:8px;padding:7px 12px;font-size:16px;color:#41455a;font-weight:500}
.chip .i{width:16px;height:16px;border-radius:4px;background:#5a50c8;opacity:.85}
.chip .i.g{background:#2f855a}
.file{display:flex;align-items:center;gap:14px;margin-top:14px;border:1px solid #e4e3ef;
  border-radius:10px;padding:14px 16px;max-width:560px;background:#fcfcff}
.file .ic{width:40px;height:40px;border-radius:8px;background:#edecf8;color:#5a50c8;display:flex;
  align-items:center;justify-content:center;font-size:13px;font-weight:700}
.file .fn{font-size:17.5px;font-weight:700}
.file .fm{font-size:14.5px;color:#8a8d9b;margin-top:2px}
.btns{display:flex;gap:10px;margin-top:16px}
.btn{font-size:16.5px;font-weight:700;border-radius:5px;padding:10px 18px}
.btn.p{background:#0b7a5c;color:#fff}
.btn.s{background:#fff;border:1px solid #d6d6e0;color:#41455a}
.note{font-size:15px;color:#8a8d9b;margin-top:12px}
.quote{border-left:3px solid #d9d7ea;padding-left:16px;margin-top:12px;color:#41455a;font-size:18.5px;line-height:1.55}
.divider{height:1px;background:#f0f0f5;margin:4px 28px}
.tiny{font-size:14px;color:#8a8d9b;padding:10px 28px 0}
.split{display:flex;flex:1;min-height:0}
.split .left{flex:1;min-width:0;display:flex;flex-direction:column}
.split .right{width:452px;flex:none;border-left:1px solid #ededf2;background:#fbfbfe;padding:22px 24px}
.pan h4{font-size:14px;font-weight:700;letter-spacing:.06em;text-transform:uppercase;color:#8a8d9b;margin:0 0 12px}
.pan h4+.row{border-top:none}
.pan .hd2{display:flex;align-items:center;gap:9px;margin-bottom:20px}
.pan .hd2 .t{font-size:19px;font-weight:700}
.pan .hd2 .c{font-size:16px;color:#5a50c8;font-weight:700;background:#edecf8;border-radius:6px;padding:3px 9px}
.row{display:flex;align-items:center;justify-content:space-between;gap:12px;padding:11px 0;
  border-top:1px solid #eeeef4;font-size:16.5px;color:#2a2d3d}
.row .off{color:#9093a2}
.tg{width:40px;height:23px;border-radius:12px;background:#d9d9e3;position:relative;flex:none}
.tg.on{background:#5a50c8}
.tg i{position:absolute;top:3px;left:3px;width:17px;height:17px;border-radius:50%;background:#fff;
  box-shadow:0 1px 2px rgba(0,0,0,.2)}
.tg.on i{left:20px}
.val{font-size:15px;font-weight:600;color:#41455a;background:#efeff5;border-radius:6px;padding:4px 9px}
.pan .note2{font-size:14.5px;line-height:1.45;color:#8a8d9b;margin-top:18px;padding-top:16px;
  border-top:1px solid #eeeef4}
.job{margin-top:14px;border:1px solid #e4e3ef;border-radius:12px;overflow:hidden;max-width:900px}
.job .jh{display:flex;align-items:center;gap:10px;background:#fafaff;border-bottom:1px solid #edecf5;
  padding:11px 16px;font-size:15.5px;font-weight:700;color:#41455a}
.job .jh .tag{font-size:12.5px;font-weight:700;letter-spacing:.04em;text-transform:uppercase;
  background:#e7f4ee;color:#1c6b4f;border-radius:4px;padding:2px 7px}
.job .jb{padding:12px 16px;font-size:16.5px;line-height:1.55;color:#2a2d3d}
.job .st{display:flex;align-items:center;gap:9px;padding:3px 0}
.job .st .k{width:18px;height:18px;border-radius:50%;background:#1c6b4f;color:#fff;font-size:11px;
  display:flex;align-items:center;justify-content:center;font-weight:700;flex:none}
.job .st .k.p{background:#5a50c8}
"""

def av(initials, color):
    return f'<div class="av" style="background:{color}">{initials}</div>'

APP_AV = f'<div class="av app">{MARK}</div>'

def msg(avatar, name, time, body, badge=False):
    b = '<span class="badge">App</span>' if badge else ''
    return (f'<div class="msg">{avatar}<div><div class="hd"><span class="nm">{name}</span>{b}'
            f'<span class="tm">{time}</span></div><div class="bd">{body}</div></div></div>')

def page(title, sub, chan, chan_meta, msgs, tiny=""):
    tinyhtml = f'<div class="tiny">{tiny}</div>' if tiny else ""
    return f"""<html><head><meta charset="utf-8"><style>{CSS}</style></head><body><div class="wrap">
<div class="top"><div><h1>{title}</h1><div class="sub">{sub}</div></div>
<div class="lock"><div class="m">{MARK}</div><div class="n">Attest Tag</div></div></div>
<div class="card"><div class="chan"><span class="h">{chan}</span><span class="dot"></span>
<span class="meta">{chan_meta}</span></div><div class="msgs">{msgs}</div>{tinyhtml}</div></div></body></html>"""

P1 = av("PR", "#4a6fb5"); P2 = av("MG", "#a8563e"); P3 = av("DL", "#2f7d6b"); P4 = av("SK", "#7a4fa3")

def page_split(title, sub, chan, chan_meta, msgs, panel):
    return f"""<html><head><meta charset="utf-8"><style>{CSS}</style></head><body><div class="wrap">
<div class="top"><div><h1>{title}</h1><div class="sub">{sub}</div></div>
<div class="lock"><div class="m">{MARK}</div><div class="n">Attest Tag</div></div></div>
<div class="card"><div class="chan"><span class="h">{chan}</span><span class="dot"></span>
<span class="meta">{chan_meta}</span></div><div class="split"><div class="left"><div class="msgs">{msgs}</div></div>
<div class="right"><div class="pan">{panel}</div></div></div></div></div></body></html>"""

def row(label, on=None, value=None):
    if value is not None:
        return f'<div class="row"><span>{label}</span><span class="val">{value}</span></div>'
    cls = "tg on" if on else "tg"
    lab = label if on else f'<span class="off">{label}</span>'
    return f'<div class="row">{lab}<span class="{cls}"><i></i></span></div>'

slides = []

# 1 — answers in the thread
slides.append(("01-answers-in-thread", page(
 "Ask in the thread.<br>Answered in the thread.",
 "Mention it in any channel it has been added to. It reads the conversation first, so you never have to explain the context you are already standing in.",
 "# support", "Thread · 9 replies",
 msg(P1, "Priya Raman", "10:42 AM",
     '<span class="mention">@Attest Tag</span> we are seeing duplicate invoices on the Q3 renewals. '
     'Has this shape come up before?')
 + msg(APP_AV, "Attest Tag", "10:42 AM",
     '<div class="status"><span class="p"></span>Read 42 messages in #support · searched #billing, #incidents</div>'
     'Twice, and both times it was the retry in <code>billing-sync</code> firing before Stripe acknowledged the '
     'first call.'
     '<div class="li"><span class="b">•</span><span><b>12 Aug, #billing</b> — Marco added an idempotency key on '
     'the renewal path. Duplicates stopped that day.</span></div>'
     '<div class="li"><span class="b">•</span><span><b>3 Sep, #incidents</b> — same signature on one-off '
     'invoices, which still use the old path.</span></div>'
     'These renewals are on the patched path, so it is worth checking whether the retry budget was raised in '
     'last week&rsquo;s deploy.'
     '<div class="foot">glm-5.3-flash · 1.2k in · 340 out · $0.0012 · <a>Configure</a></div>', badge=True)
 + msg(P1, "Priya Raman", "10:44 AM", 'it was — Marco doubled it on Friday. which accounts are affected?')
 + msg(APP_AV, "Attest Tag", "10:44 AM",
     'Nine, all renewed between Friday 18:00 and this morning. Northwind, Kestrel and Aldbury were charged '
     'twice; the other six retried inside the window and only have one invoice.'
     '<div class="foot">glm-5.3-flash · 3.4k in · 210 out · $0.0021 · <a>Configure</a></div>', badge=True))))

# 2 — grounded in your docs
slides.append(("02-your-docs", page(
 "It answers from <em>your</em> documents,<br>not from the internet.",
 "Policies, runbooks, contracts and past decisions — searched on the spot and quoted back with the source attached.",
 "# people-ops", "Direct question · 2 replies",
 msg(P4, "Sana Khalid", "9:15 AM",
     '<span class="mention">@Attest Tag</span> a customer on an annual plan wants to leave in month four. '
     'What can we actually offer them?')
 + msg(APP_AV, "Attest Tag", "9:15 AM",
     '<div class="status"><span class="p"></span>Searched 412 documents · 3 matches</div>'
     'A pro-rated refund of the unused months, minus any discount that was conditional on the full year.'
     '<div class="quote">&ldquo;Annual plans may be cancelled at any time. The unused portion is refunded '
     'pro rata by whole months. Where an annual discount was applied, the refund is calculated at the '
     'monthly list price.&rdquo;</div>'
     'On a $12,000 plan four months in, that is eight months at list, not at the discounted rate.'
     '<div class="src"><div class="chip"><span class="i"></span>refund-policy.md · §3 Annual plans</div>'
     '<div class="chip"><span class="i g"></span>Finance handbook · Credits &amp; refunds</div></div>'
     '<div class="foot">glm-5.3-flash · 2.1k in · 280 out · $0.0019 · <a>Configure</a></div>', badge=True)
 + msg(P4, "Sana Khalid", "9:17 AM", 'and if they paid by invoice rather than card?')
 + msg(APP_AV, "Attest Tag", "9:17 AM",
     'The handbook sends that to finance as a credit note rather than a refund, and asks for the account '
     'owner&rsquo;s sign-off first. Nothing in the policy treats the amount differently.', badge=True))))

# 3 — long answers as a file
slides.append(("03-reports-as-files", page(
 "Reports and exports arrive as files,<br>not as a wall of text.",
 "Ask for it as a CSV, a table or a write-up. It does the work and posts the file in the thread you asked in.",
 "# growth", "Thread · 4 replies",
 msg(P3, "Dana Lowe", "4:08 PM",
     '<span class="mention">@Attest Tag</span> go through this month&rsquo;s churn threads in #support and '
     'give me the reasons as a CSV, one row per account.')
 + msg(APP_AV, "Attest Tag", "4:11 PM",
     '<div class="status"><span class="p"></span>Read 38 threads · grouped 12 reasons · wrote the file</div>'
     '38 accounts, 12 distinct reasons. Three of them cover more than half: pricing at renewal (11), '
     'no SSO on the current tier (7), and data export limits (5).'
     '<div class="file"><div class="ic">CSV</div><div><div class="fn">churn-reasons-september.csv</div>'
     '<div class="fm">38 rows · account, reason, thread link, closed by</div></div></div>'
     '<div class="foot">glm-5.3-flash · 18.4k in · 1.9k out · $0.0141 · <a>Configure</a></div>', badge=True)
 + msg(P3, "Dana Lowe", "4:13 PM", 'can you do the same for August so I can compare?')
 + msg(APP_AV, "Attest Tag", "4:15 PM",
     'August is 31 accounts, same top three, but pricing sat fourth rather than first. Same columns as '
     'September, so the two files stack.'
     '<div class="src"><div class="chip"><span class="i"></span>churn-reasons-august.csv · 31 rows</div></div>',
     badge=True))))

# 4 — routines
slides.append(("04-routines", page(
 "Standing questions<br>answer themselves.",
 "Schedule a prompt in one sentence. It posts the answer on time — or stays quiet until something crosses the bar you set.",
 "# support", "Routine · weekdays 9:00",
 msg(P2, "Marco Gatti", "Tue 5:30 PM",
     '<span class="mention">@Attest Tag</span> every weekday at 9am post a digest of this channel, and flag '
     'anything still open after 24 hours.')
 + msg(APP_AV, "Attest Tag", "Tue 5:30 PM",
     'Routine created — <b>weekdays at 9:00</b>, Europe/London. It will post here. <a style="color:#5a50c8;'
     'font-weight:600">Edit or pause</a>', badge=True)
 + '<div class="divider"></div>'
 + msg(APP_AV, "Attest Tag", "Wed 9:00 AM",
     '<b>Support digest · Wednesday</b><br>14 new threads, 11 closed, median first reply 12 minutes.'
     '<div class="li"><span class="b">•</span><span><b>Open past 24h:</b> Northwind SSO loop (3 days), '
     'Kestrel export timing out (36h).</span></div>'
     '<div class="li"><span class="b">•</span><span><b>Rising:</b> four accounts asked about the renewal '
     'price change this week.</span></div>', badge=True)
 + '<div class="divider"></div>'
 + msg(APP_AV, "Attest Tag", "Wed 9:00 PM",
     '<div class="status"><span class="p"></span>Routine · nightly VM check · 21:00 · reply only when it matters</div>'
     '<b>web-3 is at 91% CPU</b> and has been for 40 minutes. The other eleven are under 60%.'
     '<div class="note">It ran on Monday and Tuesday too and said nothing — nothing crossed the bar.</div>',
     badge=True))))

# 5 — memory
slides.append(("05-memory", page(
 "Tell it once.<br>It holds on to it.",
 "Facts you save stay with the channel. Personal notes stay with you — no teammate, admin or scheduled run can read them.",
 "# releases", "Channel memory · 6 saved",
 msg(P3, "Dana Lowe", "Mon 8:55 AM",
     '<span class="mention">@Attest Tag</span> remember for this channel: the release captain rotates every '
     'Monday, and the captain owns the hotfix call.')
 + msg(APP_AV, "Attest Tag", "Mon 8:55 AM",
     'Saved for <b>#releases</b> — it will be in mind on every answer here. '
     '<a style="color:#5a50c8;font-weight:600">Edit this memory</a>', badge=True)
 + '<div class="divider"></div>'
 + msg(P1, "Priya Raman", "Thu 11:20 PM",
     '<span class="mention">@Attest Tag</span> prod is throwing 502s — who do I wake up?')
 + msg(APP_AV, "Attest Tag", "Thu 11:20 PM",
     'Dana is release captain this week, so the hotfix call is hers. The rotation moves Monday, '
     'so tomorrow night it would be Marco.', badge=True)
 + '<div class="divider"></div>'
 + msg(P1, "Priya Raman", "Fri 9:02 AM",
     '<span class="mention">@Attest Tag</span> remember for me: I owe Sana the Q3 numbers before Tuesday')
 + msg(APP_AV, "Attest Tag", "Fri 9:02 AM",
     'Kept for you alone. It comes back on your turns in any channel or DM, and nobody else&rsquo;s question '
     'can surface it — not a teammate&rsquo;s, not an admin&rsquo;s, not a scheduled run&rsquo;s.', badge=True))))

# 6 — approvals / control
slides.append(("06-approvals", page(
 "Nothing leaves Slack<br>without a yes.",
 "Work on connected services waits for a named approver. Admins set what it may reach, channel by channel, and every run is recorded with what it cost.",
 "# billing-eng", "Approval requested · expires in 30m",
 msg(P2, "Marco Gatti", "2:04 PM",
     '<span class="mention">@Attest Tag</span> the one-off invoice path still has no idempotency key. '
     'Can you fix it the way we fixed renewals?')
 + msg(APP_AV, "Attest Tag", "2:06 PM",
     '<div class="status"><span class="p"></span>Read the renewal patch · found the one-off path</div>'
     'I can open a <b>draft pull request</b> on <code>acme/billing-sync</code> that reuses the renewal '
     'key builder for one-off invoices, and run the billing tests before pushing.'
     '<div class="src"><div class="chip"><span class="i"></span>Connection: GitHub · billing-sync</div>'
     '<div class="chip"><span class="i g"></span>Approver: Dana Lowe</div></div>'
     '<div class="btns"><div class="btn p">Approve</div><div class="btn s">Deny</div></div>'
     '<div class="note">Nothing is pushed until an approver says yes. Draft only — a person opens it for '
     'review.</div>', badge=True)
 + '<div class="divider"></div>'
 + msg(APP_AV, "Attest Tag", "2:19 PM",
     'Approved by <b>Dana Lowe</b>. Tests ran green (41 passed), branch pushed, draft PR #214 opened — '
     'nobody merges it but you.'
     '<div class="foot">qwen3-coder · 61.2k in · 8.4k out · $0.0312 · 13m 4s · <a>Open the run</a></div>',
     badge=True))))

# 7 — end to end: find it, fix it, prove it
slides.append(("07-end-to-end-fix", page(
 "It finds the bug, opens the PR,<br>and checks the fix landed.",
 "From the first report in the channel to a verified deploy. The work happens in the thread, and a person still merges it.",
 "# incidents", "Thread · 11 replies · fix job",
 msg(P2, "Marco Gatti", "1:37 PM",
     '<span class="mention">@Attest Tag</span> checkout has been 500ing for annual plans since this '
     'morning. Find it and fix it if you can.')
 + msg(APP_AV, "Attest Tag", "1:41 PM",
     '<div class="status"><span class="p"></span>Read #incidents · 3 deploys since 08:00 · searched the repo</div>'
     'Found it. <code>applyAnnualDiscount</code> divides by <code>plan.termMonths</code>, null on the 9 plans '
     'migrated on Tuesday. Every 500 carries that stack.', badge=True)
 + msg(APP_AV, "Attest Tag", "1:52 PM",
     '<div class="job"><div class="jh">Fix job · acme/billing-sync<span class="tag">Draft PR #218</span></div>'
     '<div class="jb">'
     '<div class="st"><span class="k">1</span><code>fix/annual-term-null</code> — falls back to the billing '
     'interval when the term is missing</div>'
     '<div class="st"><span class="k">2</span>41 billing tests pass, plus a new case for a migrated plan</div>'
     '</div></div>'
     '<div class="foot">Pushed as a draft · approved by Dana · merged by Marco · qwen3-coder · $0.0381 · '
     '11m 6s · <a>Open the run</a></div>',
     badge=True)
 + msg(APP_AV, "Attest Tag", "2:34 PM",
     '<div class="status"><span class="p"></span>Watched the deploy · re-ran the failing request</div>'
     'Live on prod since 14:29 — all 9 migrated plans check out, and annual checkout has thrown zero 500s '
     'in the 20 minutes since.', badge=True))))

# 8 — per-channel permissions
PANEL = ('<div class="hd2"><span class="t">Configure</span><span class="c"># general</span></div>'
 + '<h4>What it can reach here</h4>'
 + row("Documents", on=True) + row("Web search", on=True)
 + row("GitHub · billing-sync", on=False) + row("Google Drive", on=False) + row("ClickUp", on=False)
 + '<h4 style="margin-top:22px">How it behaves here</h4>'
 + row("Answer without a mention", on=False) + row("Channel memory", on=True)
 + row("Model", value="glm-5.3-flash")
 + '<div class="note2">Set per channel by an admin. #billing-eng has GitHub switched on and a coding model; '
   'this channel has neither.</div>')

slides.append(("08-per-channel-permissions", page_split(
 "Every channel gets<br>its own permissions.",
 "What it may reach, how it behaves and which model it runs on are decided channel by channel — never once for the whole workspace.",
 "# general", "Configure · admin-set",
 msg(P1, "Priya Raman", "11:04 AM",
     '<span class="mention">@Attest Tag</span> open a PR on billing-sync that bumps the checkout timeout')
 + msg(APP_AV, "Attest Tag", "11:04 AM",
     'Not from here. The GitHub connection is switched on in <b>#billing-eng</b> and not in this channel, '
     'so I cannot touch that repository from #general. Ask me there and I will do it — or an admin can turn '
     'it on here.', badge=True)
 + msg(P1, "Priya Raman", "11:05 AM", 'fair. what can you do in this channel?')
 + msg(APP_AV, "Attest Tag", "11:05 AM",
     'Read this channel, search our documents, search the web, and remember things for #general. That is '
     'everything that is on here &rarr;', badge=True)
 + msg(P1, "Priya Raman", "11:06 AM", 'and who can change that?')
 + msg(APP_AV, "Attest Tag", "11:06 AM",
     'A workspace admin, in the console — or from the <b>Configure</b> link under any of my replies here. '
     'Each channel is set on its own.', badge=True),
 PANEL)))

# 9 — models and budget per channel
PANEL9 = ('<div class="hd2"><span class="t">Models</span><span class="c"># billing-eng</span></div>'
 + '<h4>Which model does what</h4>'
 + row("Answering", value="glm-5.3-flash")
 + row("Digging", value="deepseek-v3.2")
 + row("Fix jobs", value="qwen3-coder")
 + row("Embeddings", value="bge-m3")
 + '<h4 style="margin-top:22px">What it may spend</h4>'
 + row("This channel, monthly", value="$40.00")
 + row("One fix job", value="$3.00 cap")
 + row("Per person, hourly", value="20 turns")
 + '<div class="note2">Any OpenAI-compatible endpoint on your own key — OpenRouter, a hosted provider, '
   'or your own server. Channels choose from the models an admin allows.</div>')

slides.append(("09-models-and-budget", page_split(
 "A cheap model for chat.<br>A heavy one for digging.",
 "Each channel picks the model that fits the work and carries its own budget — so a channel that runs out stops on its own, and nothing else does.",
 "# billing-eng", "Models &amp; budget · admin-set",
 msg(P2, "Marco Gatti", "4:02 PM", '!usage')
 + msg(APP_AV, "Attest Tag", "4:02 PM",
     '<b>September so far — $18.42 of the $250 workspace budget.</b>'
     '<div class="li"><span class="b">•</span><span><b>#billing-eng</b> $6.88 of $40 — $4.20 of it fix jobs '
     'on qwen3-coder</span></div>'
     '<div class="li"><span class="b">•</span><span><b>#support</b> $7.10 of $50 — 412 answers on '
     'glm-5.3-flash</span></div>'
     '<div class="li"><span class="b">•</span><span><b>#general</b> $1.04, no cap set</span></div>'
     'A channel that reaches its cap stops answering there and says so. Every other channel keeps going.',
     badge=True)
 + msg(P2, "Marco Gatti", "4:03 PM", 'and the deep investigations? those looked expensive')
 + msg(APP_AV, "Attest Tag", "4:03 PM",
     'Those run on the heavy model, and only when a question actually needs digging — 3 this month, '
     '$2.11 between them. Ordinary answers here stay on the cheap one &rarr;', badge=True),
 PANEL9)))

OUT.mkdir(parents=True, exist_ok=True)
for name, h in slides:
    (OUT / f"{name}.html").write_text(h)
print("\n".join(n for n, _ in slides))
