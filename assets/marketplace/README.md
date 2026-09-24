# Marketplace listing images

Nine 1600×1000 PNGs for the Slack Marketplace app detail page. Slack takes **six**, shows them as a
carousel, and the first one carries the listing — it is the one most people see. The other three are
alternates.

Upload in this order:

| # | file | what it shows |
|---|---|---|
| 1 | `01-answers-in-thread.png` | a mention in #support answered in the thread, with the status line and the cost footer |
| 2 | `02-your-docs.png` | a policy question answered from the document corpus, quoted, with sources attached |
| 3 | `07-end-to-end-fix.png` | a bug found, a draft PR opened, and the fix confirmed live after the deploy |
| 4 | `08-per-channel-permissions.png` | what it may do in #billing-eng and may not in #general, with that channel's switches beside it |
| 5 | `09-models-and-budget.png` | `!usage` by channel against its budget, and which model each kind of work runs on |
| 6 | `04-routines.png` | a scheduled digest, and a second routine that only speaks when something crosses the bar |

Alternates:

| file | what it shows |
|---|---|
| `03-reports-as-files.png` | an export asked for in words, delivered as a CSV in the thread |
| `05-memory.png` | a channel memory used days later, and a personal note that stays private |
| `06-approvals.png` | an approval card on its own — mostly covered by `07-end-to-end-fix.png` |

These are **mockups**, not screen captures: the chrome is drawn rather than photographed, and the
message content follows what the app does, with some liberties that should be fixed before they are
relied on. The reply footers name a model (`glm-5.3-flash`) where the app names the tier (*basic*,
*advanced* or *custom*); `09-models-and-budget.png` shows a separate "Digging" model and says
investigations run on the heavy model, where they run on the thread's or channel's own model, and its
`!usage` reply is laid out differently from the real one; and `06-approvals.png` says the request
expires in 30 minutes, which matches nothing — a Confirm card lasts five minutes and an approver's
request a week. If you would rather ship real captures, take them in a workspace with the app
installed at 1600×1000 and keep the same order.

## Re-rendering

`source/gen.py` writes one HTML file per image; `source/shot.sh` renders each with headless Chrome at
2× and downsamples to 1600×1000 with `sips`. Edit the copy in `gen.py`, then:

```bash
cd assets/marketplace/source && python3 gen.py && zsh shot.sh
```

The HTML goes to `$TMPDIR/attesttag-mkt`, which `gen.py` creates, and the PNGs next to this README,
so the scripts move with the folder. To put the HTML elsewhere, set `D` to the folder for both:
`D=/some/where python3 gen.py && D=/some/where zsh shot.sh`. `shot.sh` needs Chrome in
`/Applications` and `sips`, so it runs on macOS only. Headline, sub-headline and every
message live in the `slides` list at the bottom of `gen.py`; the design system is the `CSS` string
above it. Brand colours come from the website: `#5a50c8` primary, `#191c2b` ink.
