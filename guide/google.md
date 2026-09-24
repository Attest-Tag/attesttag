# Google: everyone's own mail and calendar, and a Drive corpus

There are two Google integrations and they are not the same thing. Setting up the wrong one is
the most common way to spend an afternoon, so start here:

| | [**Google Workspace**](#google-workspace) | [**Drive sync**](#drive-sync) |
|---|---|---|
| answers | *what's on my calendar tomorrow?* | *what does the refund policy say?* |
| credential | an **OAuth client**, and a sign-in per person | a **service account**, one JSON key |
| whose data | each person's own, and nobody else's | one set of folders, the same for the whole organisation |
| what it does | calls Gmail, Calendar, Drive and Contacts while answering | copies files into Documents and keeps them in step |
| set up by | an admin once, then everyone connects themselves | an admin, once |
| console page | Access bundles → Connect | Documents → Google Drive |

They can both be on at once, and on a big Workspace they usually are. What they must not be is
confused: a personal sign-in is the wrong shape for a company handbook that everyone should get
the same answer from, and a service account cannot read anybody's mailbox.

> **Both of them can touch Drive, and they are still not the same thing.** Google Workspace has a
> **Drive** part that reaches *the asker's own* files, for *find my Q3 doc* — a different set of
> files for each person, nothing copied, nothing indexed. **Drive sync** mirrors *one shared
> folder* into Documents so the whole organisation gets the same answer out of the index. Wanting
> "the bot to know our handbook" is the second one; wanting "the bot to find my file" is the
> first.

---

# Google Workspace

Mail, calendar and contacts, always as the person who asked.

Most connections are one credential an admin pastes and a whole channel spends. That is the
wrong shape for a mailbox: the answer to *what is on my calendar* depends on who asked, and one
person must never read another's mail through a shared token. So the admin registers an OAuth
client, and everyone signs in for themselves. The client id and secret reach nobody's account
on their own.

- [1. Register an OAuth client](#1-register-an-oauth-client)
- [2. Add the connection and pick the parts](#2-add-the-connection-and-pick-the-parts)
- [3. Attach it where it should reach](#3-attach-it-where-it-should-reach)
- [4. Everyone connects themselves](#4-everyone-connects-themselves)
- [What writes do](#what-writes-do)
- [When it does not work](#when-it-does-not-work)

## 1. Register an OAuth client

In [Google Cloud](https://console.cloud.google.com) → APIs and Services → **Credentials** →
Create credentials → **OAuth client ID**, type **Web application**.

The **Authorised redirect URI** is your deployment's own:

```
https://YOUR_PUBLIC_HOST/connect/callback
```

Google matches it character for character. The console shows the exact string with a copy button
in the Connect dialog, built from the deployment's public origin rather than from whichever host
your browser happened to use — so copy it from there rather than typing it.

Enable the APIs for the parts you mean to use, on the same project: **Gmail API**, **Google
Calendar API**, **Google Drive API**, **People API**.

Set the consent screen to **Internal** if this is a Workspace domain. An *External* app using the
Gmail scopes needs Google's verification and an annual third-party security assessment before
anyone outside your test users can sign in — which is a project, not a step.

## 2. Add the connection and pick the parts

Console → **Access bundles** → Connect → **Google Workspace**. Paste the client id and secret.

One sign-in covers the whole connection, so the dialog is where you decide what that sign-in
asks for:

| part | read | write |
|---|---|---|
| **Gmail** | search and read the asker's own mail | *write drafts and send mail as you* — `gmail.compose` is drafts **and** sending, because Google has no draft-only scope. The bot is told to draft, and a send would be held for Confirm like any other write, but the grant is wider than the bot's habits, and the consent screen will say so |
| **Calendar** | read calendars, and find a time everyone is free | book, move and cancel events |
| **Drive** | search and read their own files — *find the Q3 planning doc*, *what does my onboarding checklist say?* | create, edit and delete any of their files |
| **Contacts and directory** | turn a name into an address, so *Priya at Acme* is enough | — |

A part left unticked is a scope nobody is ever asked to grant and a host the proxy will not
carry, so "Calendar, read only, no mail" is refused by Google itself rather than by a rule the
model is trusted to keep. The admin's boxes are the most anyone can grant: each person's Connect
page starts with them ticked, and they can untick a part, or its writing, and grant less.

### Drive scopes and editing Google Docs

**Drive is the one part that starts unticked**, and deliberately. Every other part is bounded by
its service; a Drive grant reaches every file the person can open, which is the widest thing
anyone is asked for in this dialog. Nobody should arrive at it by accepting a default — an admin
who wants it can say so.

Its write box is wider than it looks, for a reason worth knowing before you tick it. Google has
no *edit the files they already have* scope: `drive.file` reaches only files this app itself
created, which is no use for a document somebody wrote last week. So writing means the full Drive
scope, and the consent screen will say so. Reading is `drive.readonly`. Both are **restricted**
scopes, like Gmail's — one more reason the consent screen wants to be Internal.

Editing a Google Doc is not an in-place edit, whatever it looks like from Slack: the file is
exported, the text changed, and the whole thing written back. The bot says so rather than
implying it made a surgical change, and it names the file and its link in the Confirm card first,
because two files can share a name and an edit to the wrong one does not look wrong afterwards.

Writing is scoped rather than method-gated on purpose: a read-only calendar still has to `POST`
to `freeBusy` to answer *when is everyone free*.

**Test connection** here checks only that the OAuth client is complete, and says that nothing is
reachable until somebody signs in. There is no organisation credential to try, and a green tick
that meant anything else would be a lie.

## 3. Attach it where it should reach

Console → **Workspaces** → attach the bundle to the workspace, or to one channel if it should not
reach everywhere. A connection can also be attached on its own, without the rest of its bundle.

Nothing is reachable until this is done — including for people who have already signed in.

## 4. Everyone connects themselves

In Slack, each person says **`!connect`** and gets a private message with a Connect button.
Saying it in words — *connect my Gmail* — does the same, and the first time somebody asks about
their own calendar the bot sends them the same link unasked.

The link is a capability minted for one person and one connection, good for **thirty minutes**,
and it never goes near the model — a tool result is prompt text, and prompt text travels to
whoever serves the model. Their refresh token is sealed against their Slack identity and used
only for calls they themselves set off, which includes the routines they created.

The same page disconnects. Disconnecting hands the grant back to Google rather than only
forgetting it here. In the console, an `oauth_user` connection shows who has connected; the row
never carries a token. Deleting the connection revokes every personal grant under it the same
way — a token nobody can see in the console any more is still a live grant at the provider.

A call the bot makes on somebody's own account is on the Activity page behind a lock: whose account
it was, the connection, and for a request its method, its endpoint without the query string, the
status and the size of the reply — never what was asked or what came back, not even to an admin,
because that page is open to everyone who can view activity and the answer is that person's own
mail or calendar. A `run_js` script whose `fetch()` went through their account is held to the same
rule, and keeps only whose account it was and the size of its answer.

`!personal_instructions <text>` — or the Configure page's *Tools and access → Your accounts*
tab — tells the bot how to go about things when it acts as you: *only ever look at my inbox*,
*sign my drafts off with my first name*. They reach it on your own turns and nobody else's, and
`!personal_instructions clear` removes them.

If nobody has set any of this up and somebody asks about their calendar anyway, the bot posts
the setup steps into the thread — with the real console links and the real redirect URI — rather
than inventing an answer. It is usually not the asker who can act on them, which is why it goes
in the thread rather than in a DM.

## What writes do

*Find half an hour with Priya on Thursday and book it* reads `freeBusy` freely — the proxy knows
that path is a read despite answering to `POST`, because a Confirm card people press in order to
*look* at something teaches them to press Confirm without reading — and then shows the event it
means to create with **Confirm** and **Cancel** under it, answering with the Meet link once it
has run.

Name the hour yourself and it skips the looking: *set up a call at 7am with everyone on this
thread* goes straight to the same card, with the thread's own people as the attendees. A name
rather than an address — *Orle from ClearOne* — is a contacts search first.

Drive writes are held the same way, and reads are not: searching Drive and reading a file happen
without asking, while creating, editing, renaming or deleting one shows the Confirm card naming
the file it means to change.

An approver may press Confirm on somebody else's write; it still runs out of the asker's
account, never the approver's. Pending writes expire after five minutes.

## When it does not work

| what you see | what it is |
|---|---|
| `redirect_uri_mismatch` | the URI registered with Google is not the one the deployment builds. Copy it from the Connect dialog, and check `ADMIN_BASE_URL` if the origin is wrong |
| Google refuses the scopes at sign-in | an External consent screen with Gmail scopes, before verification. Set it to Internal, or drop the Gmail part |
| a call is refused for a part you ticked | the API is not enabled on the project — Gmail, Calendar, Drive and People are four separate switches |
| `!connect` says this channel hasn't been given that connection yet | the link still goes out, since it connects the person's own account, but the bundle is not attached to this channel or the workspace. See [step 3](#3-attach-it-where-it-should-reach) |
| `!connect` says nothing here runs on your own account yet, with setup steps | the organisation has no connection that runs on people's own accounts anywhere. See [step 2](#2-add-the-connection-and-pick-the-parts) |
| the first contacts search finds nobody | Google's search endpoints are cache-backed and answer the very first query of a session with nothing. The bot warms them and retries; a person doing it by hand has to send the query once empty first |
| Drive search finds nothing that is obviously there | the file is on a **shared drive**. Drive returns only My Drive unless a search asks for shared drives explicitly; the bot is told to, but a hand-written call has to pass `supportsAllDrives=true&includeItemsFromAllDrives=true` |
| the Drive boxes are not ticked after an upgrade | Drive is off by default, including for connections that existed before it was added. Tick it and everyone re-consents |
| an event books but has no Meet link | `conferenceDataVersion=1` was missing — Google drops `conferenceData` and returns a perfectly successful event with no video link |
| a colleague shows as free when they are not | their calendar is not shared. `freeBusy` returns an error for that entry, which is reported rather than read as free |

---

# Drive sync

A folder in Google Drive, copied into Documents and kept in step with it.

This is deliberately not the Drive *connection*, which lets the bot ask Drive a question while
it is answering. A sync takes copies, so the text is in the index and every answer can cite it
without a round trip — and so an answer still works when Drive is slow, or when the question was
too vague for the model to have searched Drive well. A connection is better for *find me the
deck Priya shared last week*; a sync is better for the twenty documents the bot should already
know.

- [1. Make a service account](#1-make-a-service-account)
- [2. Share the folder with it](#2-share-the-folder-with-it)
- [3. Add the credential](#3-add-the-credential)
- [4. Add the folder](#4-add-the-folder)
- [What comes in, and what does not](#what-comes-in-and-what-does-not)
- [The mirror](#the-mirror)
- [Limits](#limits)
- [When it does not work](#when-it-does-not-work-1)

## 1. Make a service account

In [Google Cloud](https://console.cloud.google.com) → IAM and Admin → Service Accounts, create
one, then Keys → Add key → Create new key → **JSON**. That file is the whole credential; it is
downloaded once and cannot be downloaded again.

Enable the **Google Drive API** on the same project (APIs and Services → Library). A key on a
project without the API enabled authenticates fine and then fails every call.

The service account needs no IAM roles for this. What it can read is decided entirely by Drive
sharing, which is the next step.

## 2. Share the folder with it

Open the JSON key and find `client_email` — something like
`attest-docs@my-project.iam.gserviceaccount.com`. In Drive, open the folder, Share, paste that
address, **Viewer**, send.

> **The trap.** A service account is not a member of your domain. A folder shared *with everyone
> at acme.com*, or set to *anyone with the link inside the organisation*, is **invisible** to it —
> the call succeeds and the folder comes back empty. It has to be shared with that address
> specifically. This is the single most common reason a sync finds nothing.

Folders on a **shared drive** work, and are usually where a company's documents actually are.
Add the service account to the shared drive (or to the folder) as a member; the sync asks Drive
for shared-drive items explicitly, so nothing extra is needed on this side.

Viewer is enough. Drive sync only reads.

## 3. Add the credential

Console → **Access bundles** → Connect → **Google Drive**. Paste the JSON key.

Which bundle you file it under matters: the bundle decides which channels can reach the
credential for *ad-hoc* Drive questions. It does not restrict the sync — a sync is an admin's own
configuration and runs on the credential directly — so a credential that exists only to feed
Documents can sit in a bundle attached to nothing at all.

**Test connection** calls `/drive/v3/about?fields=user` and hands back the account it
authenticated as, which is a quick way to confirm you shared the folder with the right address.

A **Google Workspace** connection will not appear in this picker even though it reaches the same
host. Its credential is one token per person, and a sync runs unattended on behalf of nobody in
particular — so it is turned away here rather than failing every six hours.

## 4. Add the folder

Console → **Documents** → the **Google Drive** panel → **Add folder**.

| field | what it takes |
|---|---|
| Credential | `bundle / name` of a connection that reaches Drive and has a secret |
| Drive folder | paste the link straight from Drive's address bar — `/folders/<id>`, an `open?id=…` link, or a bare id all work |
| Lands in | the Documents folder it goes under: top level, or one you have already made. Drive's own subfolders keep their shape underneath it |
| Scope | the whole workspace, or one channel. Set here rather than on the documents, which are rewritten each pass |
| Include subfolders | off means only the files sitting directly in the folder |

**Test** walks the folder and draws what a pass would take — the tree, the name each file would
have as a document, what would be skipped and why, and the totals. It downloads nothing and
writes nothing, and it uses the same walk and the same rules as a real pass, so it cannot promise
a file the pass then refuses. Use it before saving.

**Add folder** checks the folder before it saves: a link that turns out to be a file, a folder in
the trash, or one the service account was never shared with is turned away with the reason while
you are still looking at the dialog, rather than six hours later in a run log.

After that a pass runs **every six hours**, and **Sync now** runs one folder — or all of them,
from the panel header — straight away. The scheduled loop deliberately does not run at boot: a
container that restarts often would re-sync on every restart, and the documents were already
there a moment ago. The first scheduled pass is one interval in, which is what Sync now is for.

Anything that changed triggers a reindex on its own. Adding a sync needs the **Documents:
manage** permission; the credential picker also needs **Connections: view**.

## What comes in, and what does not

Google-native files have no bytes to download, only an export:

| in Drive | becomes |
|---|---|
| Google Doc | `.md` |
| Google Sheet | `.csv` — Drive exports the **first sheet only**, which is its rule and not ours, and the reason a multi-tab sheet is worth keeping as a real `.csv` per tab |
| Google Slides | `.pdf` |
| anything else Google-native (Forms, Drawings, …) | skipped — no text to export |

An uploaded file keeps its own name when that name is already a type the index reads: `.md`,
`.markdown`, `.txt`, `.rst`, `.pdf`, `.html`, `.htm`, `.csv`, `.json`. Anything else is skipped
with a note in the report.

A Drive title is made safe as one path segment on the way in: a slash becomes `-` (Drive allows
a slash in a title and the documents tree would read it as a folder), leading dots are trimmed
(a dot file is never indexed, and a title beginning with one is far likelier to be a title than
an intent to hide), and a very long title is truncated.

## The mirror

Every document a sync creates is recorded against that sync. That record is what makes the
mirror honest:

- A file that stops coming back from Drive is **removed** from Documents.
- A file **renamed or moved** between subfolders is the same file under a new name, so the old
  document goes rather than being left as a duplicate.
- A document with **no** such record was put there by a person, and this code never touches it —
  an upload can sit in the same folder as a synced document and survive every pass.
- **Deleting a sync leaves the documents.** Stopping a sync is not a reason to take away what the
  bot already answers from; anything unwanted can be deleted in the table like any other
  document. Deleting the *credential* drops the syncs that spent it, on the same principle that
  they cannot run any more.

A folder that has been deleted in Drive fails the pass rather than reading as *every file is
gone, mirror it*. Same for a folder that has become a file, or landed in the trash.

## Limits

| | |
|---|---|
| one file | 25 MB — checked against Drive's reported size *and* while reading, because a native export has no size until it is made |
| one folder listing | 2,000 files, in name order so a capped folder takes the same files every pass |
| folders walked | 200 |
| preview tree | 2,000 entries drawn; the rest are counted and reported as *more* |
| syncs per organisation | 25 (`LIMIT_DRIVE_SYNCS_PER_ORG`) |

Passes run one at a time across the whole deployment. A pass is mostly waiting on Drive and then
on the embedding endpoint, and running thirty at once would put a deployment's entire re-indexing
budget into one minute.

## When it does not work

| what you see | what it is |
|---|---|
| the folder comes back empty, no error | shared with your domain rather than with the service account's own address. See [step 2](#2-share-the-folder-with-it) |
| `File not found` | the same thing, or a link to a folder in someone else's Drive |
| `… is a file, not a folder` | you pasted a document link. Open the folder and copy the address bar |
| an error that does **not** mention sharing | the credential itself — a key Google no longer knows, or the Drive API not enabled on the project. Sharing will not fix it, which is why the two are told apart |
| everything skipped | the folder holds Google-native types with no text export, or files the index does not read. Test shows the reason per file |
| a document you deleted keeps coming back | it is still in Drive. Delete it there, or the next pass restores it |

## See also

- [Connections](connections.md) — bundles, scopes, the proxy, and how writes are held
- [Configuration](configuration.md#documents) — how documents are indexed
- [Admin console](console.md) — the Documents and Access bundles pages
