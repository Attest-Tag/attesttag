# Microsoft Teams

attest_tag answers in Microsoft Teams the way it does in Slack: the same agent, the same
connections, approvals, budgets and routines, and the same console. What changes is how the bot
gets into an organisation, and a handful of things Teams does not let an app do — those are listed
[at the end](#what-is-different-from-slack), with the reason for each.

There are two halves, done by different people:

- **Once per deployment**, whoever runs attest_tag registers a bot with Microsoft and gives the
  deployment its credentials. A customer of the hosted service skips this half: it is the
  service's to do.
- **Once per Teams organisation**, a Teams admin uploads the app, and someone sends the bot a code
  from the console that says which attest_tag account the organisation belongs to.

- [How it fits together](#how-it-fits-together)
- [1. Register the bot](#1-register-the-bot)
- [2. Give the deployment its credentials](#2-give-the-deployment-its-credentials)
- [3. Connect a Teams organisation](#3-connect-a-teams-organisation)
- [Signing in to the console with Microsoft](#signing-in-to-the-console-with-microsoft)
- [What a Teams admin has to allow](#what-a-teams-admin-has-to-allow)
- [What is different from Slack](#what-is-different-from-slack)
- [When it does not work](#when-it-does-not-work)

## How it fits together

Microsoft delivers every message the bot can see to `https://<your host>/msteams/messages`, signed
by the Bot Framework. The signature is checked before anything in the message is believed — that
it came from Microsoft, for this bot, from a Bot Framework host — and replies go back only to
Bot Framework hosts, whatever a message claims. Microsoft ships no Go SDK, so this is the Bot
Framework's REST protocol spoken directly; there is nothing extra to run.

A connected Teams organisation (a Microsoft 365 *tenant*) is one workspace in the console, beside
any Slack workspaces, with its channels under it. Access bundles, instructions and budgets attach
to it and to its channels exactly as they do for Slack.

Unlike Slack, there is no token per install. The bot's own credentials let it answer in any
conversation it has been added to, so a Teams workspace stores nothing secret at all.

## 1. Register the bot

You need an Azure subscription and a Microsoft Entra directory to register the bot in. The Azure
Bot resource on the **F0** tier costs nothing, and Teams is one of its free channels.

1. **An app registration.** In the Azure portal, open *Microsoft Entra ID → App registrations →
   New registration*. Call it `attest_tag`. For *Supported account types*, choose *Accounts in
   any organizational directory* if organisations other than your own will install the bot, or
   *this organizational directory only* if it is for one company. No redirect URI.
2. **A client secret.** In the registration, *Certificates & secrets → New client secret*. Copy
   the *Value* now; it is shown once. Note when it expires — rotating it is a new secret and a
   restart.
3. **The two ids.** On the registration's *Overview*, copy the *Application (client) ID* and the
   *Directory (tenant) ID*.
4. **The bot.** *Create a resource → Azure Bot*. Pricing tier *Free (F0)*. *Type of App*:
   *Single Tenant*. *Creation type*: *Use existing app registration*, and paste the two ids.
5. **Where it delivers.** In the bot, *Configuration → Messaging endpoint*:
   `https://<your host>/msteams/messages`. Apply.
6. **Teams.** In the bot, *Channels → Microsoft Teams*, accept the terms, and apply.

**One bot, one address.** A bot has exactly one messaging endpoint, so a test deployment and a
production one each need their own app registration and bot, the way a test Slack app and a
production one are two apps. Nothing else in attest_tag is per host: the app package and the
sign-in redirect are made from the deployment's public origin (`ADMIN_BASE_URL`, or the address
the console was first signed in on), so a package names that host wherever it was downloaded
from, and a sign-in started on another address is sent to that origin first. Sign in with
Microsoft's redirect URIs (below) belong to the app registration, which can list several.

Microsoft stopped creating new *multi-tenant* Azure Bot resources on 31 July 2025, which is why
step 4 says Single Tenant even for a bot other organisations will use. A multi-tenant app
registration behind a single-tenant bot resource is how a new bot is meant to serve other
tenants; Microsoft's documentation does not yet say so in as many words, so try it with a second
tenant before promising it to a customer. A bot created before the cut-off as multi-tenant keeps
working: set `MSTEAMS_APP_TYPE=MultiTenant`.

## 2. Give the deployment its credentials

| variable | value |
|---|---|
| `MSTEAMS_APP_ID` | the Application (client) ID |
| `MSTEAMS_APP_PASSWORD` | the client secret's value — the only secret of the four |
| `MSTEAMS_TENANT_ID` | the Directory (tenant) ID |
| `MSTEAMS_APP_TYPE` | `SingleTenant`, unless the bot resource says otherwise |

Once `MSTEAMS_APP_ID` is set, the password is required, and so is the tenant id for a
single-tenant bot; one missing — or `MSTEAMS_SIGNIN` set without an app id — stops the process at
startup, because an id without a password is a bot that hears every message and cannot answer
one. `MSTEAMS_APP_TYPE` may be left out. The deploy scripts pass them through — the password as
a secret, the rest as plain settings, since the ids are in the app package every tenant installs
anyway. [Configuration](configuration.md#microsoft-teams-optional) has the table beside
everything else.

After a restart the log says so:

```
level=INFO msg="microsoft teams" app_id=… type=SingleTenant endpoint=/msteams/messages
```

and an unsigned request is turned away, which is the quickest way to see the endpoint is live:

```bash
curl -s -o /dev/null -w "%{http_code}\n" -X POST https://YOUR_HOST/msteams/messages -d '{}'
```

`401` is right. `503` means the process did not get the credentials.

## 3. Connect a Teams organisation

In the console, **Workspaces → Add workspace → Microsoft Teams** — or, on a new account, the first
step of the setup walk. The dialog has the three steps, each with a link to where in Microsoft's
UI it is done. The same steps, with a drawing of every screen, are at `/admin/help/teams/` on every
deployment; that page needs no sign-in, so it can be sent to the Teams admin who does step 1.

1. **Download the app package** and give it to a Teams admin, who uploads it in the Teams admin
   centre: *Teams apps → Manage apps → Actions → Upload new app*. That puts it in the
   organisation's own catalogue. The package is generated for this deployment: it names this
   deployment's bot and host, so a package from somebody else's deployment installs somebody
   else's bot. On a brand-new Microsoft 365 organisation the admin centre first spends up to half
   an hour "setting up your new app management experience", and uploading is greyed out until it
   has.
2. **Add the app where people will talk to it.** In Teams, *Apps → attestTag → Add to a team*,
   or open a chat with it. The app is called attestTag in Teams, so people mention it as
   `@attestTag` there. Adding it to a team asks the person doing it whether the app may read that
   team's channel messages. Say yes: without it the bot hears only the messages that @mention it.
   The team's channels then appear under the workspace in the console.
3. **Send the bot the code.** *Get a code* shows a line like `link ABCD-EFGH`. Send it to the bot
   in Teams — in a chat with it, or `@attestTag link ABCD-EFGH` in a channel. It works once, for
   thirty minutes, and joins the organisation it is sent from to this account; the bot answers
   *Connected*. Until then the bot answers a mention or a direct message with how to connect it,
   at most once every five minutes in each conversation, and nothing else.

### What the link code proves

The code is what proves the account and Microsoft's signature on the message that carries it is
what proves the organisation, which is why the code is sent from inside Teams rather than typed
into the console. The link stands only if whoever sends the code is confirmed as a member of that
organisation — not a guest, and not somebody from another tenant in a shared channel — and a
roster lookup that cannot say undoes it rather than leaving it in place. One organisation takes
at most ten tries an hour, so nobody can spray codes at a tenant they do not control. An
organisation already joined to a different attest_tag account stays where it is; an admin of that
account has to disconnect it first.

### Who may use the bot in Teams

Who may use the bot is decided as it is for Slack: the organisation's email domains (*Settings →
Security*, with `ALLOWED_EMAIL_DOMAINS` as the default for an organisation that keeps no list)
against the address Teams gives for the person, guests and anonymous users refused, and people
from other organisations refused even in a chat they share with a member — unless *Settings →
Security → Guests and other organisations* lets them in. An account starts with the company
domain it was signed up with as its list, and a Microsoft 365 organisation's addresses can be
on another one — `yourcompany.onmicrosoft.com`, on a new one. When that is so for whoever sends
the code, the bot's answer says it cannot answer them yet; add the domain there.

## Signing in to the console with Microsoft

The console can offer **Sign in with Microsoft** beside Sign in with Slack, through the same app
registration. It is off until you say who it is for:

1. On the app registration, *Authentication → Add a platform → Web*, with the redirect URI
   `https://<your host>/api/auth/microsoft/callback`.
2. Set `MSTEAMS_SIGNIN=organizations` to accept any work or school account, or to one tenant id
   to accept only that directory's. Restart.

A Microsoft sign-in proves who somebody is in their organisation — the tenant and their object
id there, which is the same pair the bot knows them by in Teams. So a person who connects it is
the same person in the console as the one the bot talks to: their private notes, and what the
console records them as having done, line up with their Teams account.

It does not prove an email address. The address in a Microsoft token is one the person's own
tenant admin can set to anything, including somebody else's, so it is never used to find, join
or confirm an account. A first Microsoft sign-in makes a new account whose address still has to
be confirmed; to use Microsoft with an account you already have, sign in the usual way and press
*Connect Microsoft* under *Settings → Security*. An organisation can then require it for
everybody with the *Microsoft only* sign-in policy, the way *Slack only* works for Slack.

Single sign-on, under the same Security tab, is the other way to use a Microsoft directory: it is
set up per organisation against the organisation's own Entra app and a domain it has proved it
owns, and it becomes the roster as well as the door.

## What a Teams admin has to allow

Send this to the customer's Teams admin. Nothing on it needs a Microsoft Graph permission or an
admin consent grant.

- **Uploading a custom app.** The package is a custom app, not a Teams Store app, so a Teams admin
  uploads it: *Teams admin centre → Teams apps → Manage apps → Actions → Upload new app*. If the
  organisation blocks custom apps, allow `attestTag` for the people who will use it.
- **Who may use it.** The app is available to whoever the organisation's app policies allow —
  everyone, or a group. The bot's own access rules apply on top.
- **Resource-specific consent for team owners.** Adding the app to a team asks the team owner to
  let it read that team's channel messages (`ChannelMessage.Read.Group`), and adding it to a group
  chat asks the same for the chat (`ChatMessage.Read.Chat`). If the organisation has switched
  resource-specific consent off, the bot works but hears only messages that @mention it.
- **Nothing else.** It asks for no Graph permission and no mailbox, and it stores no credential
  for the organisation. A file somebody sends the bot in a chat goes to their own OneDrive, as
  any file sent in a Teams chat does, and the bot is handed a link to that one file.

## What is different from Slack

| | Slack | Microsoft Teams |
|---|---|---|
| Answers in channels, threads and direct messages | yes | yes — channel reply chains, group chats and one-to-one chats |
| Hears every message in a channel, not only mentions | yes | yes, once a team owner grants it when adding the app |
| The answer appears as it is written | everywhere | in one-to-one chats; elsewhere it is posted whole when it is done |
| Shows the tool call in flight | a task card | the progress line, in one-to-one chats |
| Confirm and approval buttons | yes | yes, as Adaptive Cards |
| Access requests sent to approvers | as a DM | as a chat, to approvers who share a team with the bot or have the app |
| Reading a thread back | from Slack | from the bot's own record of the conversation, so from when it was added |
| Reacting with an emoji (`react`) | yes | no — not offered on Teams, and read-every-message there replies or stays silent, never reacts |
| Searching messages across channels (`slack_search`) | yes | no — Microsoft offers no message search to an app |
| Pins, the channel list, a person's profile (`list_pins`, `list_channels`, `get_user`) | yes | no — not offered on Teams |
| Reading a file somebody attached | yes | a file sent to the bot in a chat, and an image pasted into any message. A file attached in a channel or group chat lives in SharePoint: the model is told it is there and what it is called, because reading it needs a Graph permission this app does not ask for |
| Posting a file, an artifact among them (`create_artifact`) | yes | no — not offered on Teams; long output stays in the message, a job's diff on its page |
| Forwarding email into a channel | yes | Slack only |
| Signing in to the console | email, Sign in with Slack, SSO | email, Sign in with Microsoft, SSO |

A tool Teams cannot back is left out of the model's tool set rather than offered and made to fail:
a tool that always errors spends turns and confuses the model.

Three of these are Microsoft's to change, not attest_tag's. Streaming is one-to-one only because
that is the only place Teams allows it. Message search is not available to an application at all.
And an app can start a chat only with somebody who shares a team with it or has installed it
themselves — anyone else needs the organisation to pre-install the app for them with an app setup
policy.

## When it does not work

**Teams says *This app cannot be found*, or searching Apps finds nothing.** A new upload takes a
few minutes to reach Teams clients, and the desktop app keeps its own list until it is restarted.
Wait, reload Teams, and try again, or skip the search: the app's page in *Manage apps* ends with its
catalogue id, and `https://teams.microsoft.com/l/app/<that id>` opens the app in Teams with its Add
button. Uploading again gives the app a new id, so an old link then says the app cannot be found.
If it still isn't there, the organisation's app policies may be hiding custom apps (*Actions →
Org-wide app settings → Custom apps* in Manage apps).

**Teams says `AppSideloadingForbidden` when adding the app.** The package was added as a
personal upload, and the organisation does not let people upload custom apps for themselves.
Upload it as an admin in the Teams admin centre instead (step 1), which puts it in the
organisation's catalogue, and add it from there — or select it under *Manage apps* and use *Add to
team*.

**The bot answers nothing at all.** Look for `Teams delivery refused` in the log. Its `why` says
which check failed: a token for another bot means `MSTEAMS_APP_ID` is not the id the Azure Bot
uses, and a service URL that is not a Bot Framework host is refused whatever else is right. No
line at all means Microsoft is not calling: check the messaging endpoint on the Azure Bot, and that
its Teams channel is on.

### Link codes, email domains and refused tokens

**It answers *I'm not connected to an attest_tag account in this organisation yet*.** Send it a
code from the console.

**It answers *That code didn't work*.** Codes last thirty minutes and work once. Get another.

**It answers *Only a member of this Microsoft Teams organisation can connect it*.** Whoever sent
the code is a guest, belongs to another organisation, or the roster could not confirm them just
then. A member of the organisation sends a new code.

**It answers *Too many attempts to connect this Microsoft Teams organisation just now*.** One
organisation takes ten tries an hour. Wait, then send a code again — a new one if thirty minutes
have passed.

**It answers that the organisation is already connected to a different account.** It is. An admin
of that account disconnects it under Workspaces, and then a new code works.

**It answers *I only work with … accounts*.** The address Teams has for that person is not on the
organisation's email domains. Add the domain under *Settings → Security → Email domains*, or empty
the list, which lets every member of the organisation in unless the deployment sets
`ALLOWED_EMAIL_DOMAINS`.

**It answers @mentions in a channel but not other messages.** The permission to read the team's
channel messages was not granted. Remove the app from the team and add it again, saying yes this
time, or ask the Teams admin whether resource-specific consent is switched off.

**Replies fail with `Microsoft refused a token`.** The line carries Microsoft's reason.
`AADSTS7000215` is a wrong or expired `MSTEAMS_APP_PASSWORD`; `AADSTS700016` means
`MSTEAMS_TENANT_ID` is not the directory the app registration is in.
