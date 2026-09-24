"use client";

import { useState } from "react";
import Link from "next/link";
import { ExternalLink } from "lucide-react";
import { CopyButton } from "@/components/core/copy-button";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  FindAppScreen,
  LinkChatScreen,
  MentionScreen,
  TeamAppsScreen,
  UploadDialogScreen,
  UploadMenuScreen,
} from "@/components/help/teams-screens";
import { BrandMark } from "@/components/shell/brand-mark";
import { useAuth } from "@/components/shell/auth-provider";
import { TEAMS_APP_NAME, TEAMS_MANAGE_APPS_URL, TEAMS_URL } from "@/lib/msteams";

// Connecting Microsoft Teams, one screen at a time. The connect dialog says what to do in three
// lines; this is the same three steps for somebody who has never opened the Teams admin centre,
// and it is public because the person doing step 2 is often an IT admin with no account here —
// the dialog tells people to send them this page.
//
// Nothing on it is specific to one deployment or one organisation: the file it talks about comes
// from the console, and the code from the console too.

function Out({ href, children }: { href: string; children: React.ReactNode }) {
  return (
    <a
      href={href}
      target="_blank"
      rel="noreferrer"
      className="inline-flex items-center gap-1 font-medium text-primary underline-offset-2 hover:underline"
    >
      {children}
      <ExternalLink className="size-3.5" />
    </a>
  );
}

function Step({
  n,
  id,
  title,
  who,
  children,
}: {
  n: number;
  id: string;
  title: string;
  who?: string;
  children: React.ReactNode;
}) {
  return (
    <section id={id} className="scroll-mt-6 space-y-4">
      <div className="flex items-start gap-3">
        <span className="mt-0.5 grid size-7 shrink-0 place-items-center rounded-full bg-primary text-sm font-semibold text-primary-foreground">
          {n}
        </span>
        <div>
          <h2 className="text-lg font-semibold leading-tight">{title}</h2>
          {who && <p className="mt-0.5 text-sm text-muted-foreground">{who}</p>}
        </div>
      </div>
      <div className="space-y-4 pl-10">{children}</div>
    </section>
  );
}

function Figure({ caption, children }: { caption: React.ReactNode; children: React.ReactNode }) {
  return (
    <figure className="space-y-2">
      {children}
      <figcaption className="text-xs text-muted-foreground">{caption}</figcaption>
    </figure>
  );
}

function Note({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="rounded-lg border bg-card px-4 py-3 text-sm">
      <p className="font-medium">{title}</p>
      <div className="mt-1 text-muted-foreground">{children}</div>
    </div>
  );
}

const K = ({ children }: { children: React.ReactNode }) => (
  <span className="font-medium text-foreground">{children}</span>
);

export function TeamsGuide() {
  const { me } = useAuth();
  const signedIn = !!me?.signed_in;
  const bot = TEAMS_APP_NAME;

  return (
    <div className="min-h-dvh bg-background">
      <header className="border-b">
        <div className="mx-auto flex max-w-3xl items-center gap-3 px-4 py-3">
          <Link href="/" className="flex items-center gap-2 font-semibold">
            <BrandMark size="sm" /> attest_tag
          </Link>
          <span className="text-sm text-muted-foreground">Help</span>
          <Link href="/" className="ml-auto text-sm font-medium text-primary hover:underline">
            {signedIn ? "Back to the console" : "Sign in"}
          </Link>
        </div>
      </header>

      <main className="mx-auto max-w-3xl space-y-10 px-4 py-8">
        <div className="space-y-3">
          <h1 className="text-2xl font-semibold tracking-tight">Connect {bot} to Microsoft Teams</h1>
          <p className="text-muted-foreground">
            Four steps, about ten minutes. Step 2 needs a Teams administrator for your organisation:
            if that is not you, send them this page and the app file from step 1.
          </p>
          <nav aria-label="Steps" className="flex flex-wrap gap-x-4 gap-y-1 text-sm">
            <a href="#file" className="text-primary hover:underline">1. The app file</a>
            <a href="#upload" className="text-primary hover:underline">2. Upload it</a>
            <a href="#add" className="text-primary hover:underline">3. Add it in Teams</a>
            <a href="#code" className="text-primary hover:underline">4. Send the code</a>
            <a href="#trouble" className="text-primary hover:underline">If it does not work</a>
          </nav>
        </div>

        <Step n={1} id="file" title="Get the app file" who="Anyone who manages the attest_tag console">
          <p className="text-sm">
            In the attest_tag console, open <K>Workspaces</K>, choose <K>Add workspace → Microsoft Teams</K>,
            and press <K>Download the Teams app</K>. You get a small zip file,{" "}
            <span className="font-mono text-xs">attest_tag-teams.zip</span>. Don&apos;t unzip it: Teams
            takes the zip as it is.
          </p>
          <p className="text-sm text-muted-foreground">
            The file is made for your attest_tag: it names your bot, so a file from anywhere else would
            install somebody else&apos;s.
          </p>
        </Step>

        <Step n={2} id="upload" title="Upload it in the Teams admin centre" who="A Teams administrator, once for the whole organisation">
          <p className="text-sm">
            Open <Out href={TEAMS_MANAGE_APPS_URL}>Manage apps in the Teams admin centre</Out>. At the top
            right, press <K>Actions</K>, then <K>Upload new app</K>.
          </p>
          <Figure caption={<>① Actions, at the top right of Manage apps. ② Upload new app.</>}>
            <UploadMenuScreen />
          </Figure>
          <p className="text-sm">
            In the box that opens, press <K>Upload</K> and choose the zip file. When it finishes,{" "}
            {bot} is in your organisation&apos;s own app store, and everyone the organisation&apos;s app
            policies allow can add it.
          </p>
          <Figure caption={<>③ Upload, then pick attest_tag-teams.zip.</>}>
            <UploadDialogScreen />
          </Figure>
          <Note title="Upload is greyed out, or missing">
            A brand-new Microsoft 365 organisation spends up to half an hour &ldquo;setting up your new
            app management experience&rdquo;, and uploading waits until it has. Otherwise, your account
            needs the Teams Administrator or Global Administrator role.
          </Note>
        </Step>

        <Step n={3} id="add" title={`Add ${bot} in Teams`} who="Anyone in the organisation">
          <p className="text-sm">
            Open <Out href={TEAMS_URL}>Teams</Out>, press <K>Apps</K> in the left-hand rail, and search
            for <K>{bot}</K>. Open it and press <K>Add</K>: that starts a chat between you and the bot.
            (A Teams link first asks whether to open the desktop app; <K>Use the web app instead</K> works
            just as well.)
          </p>
          <Figure caption={<>① Apps. ② Search for {bot}. ③ Add.</>}>
            <FindAppScreen />
          </Figure>
          <Note title="Not there yet?">
            <p>
              A newly uploaded app takes a few minutes to reach Teams, and until it has, Teams says
              &ldquo;This app cannot be found&rdquo; and the search comes back empty. Wait a few minutes and
              reload Teams; the desktop app keeps its own list, so quit it and open it again.
            </p>
            <p className="mt-2">
              Or skip the search with a direct link. In <Out href={TEAMS_MANAGE_APPS_URL}>Manage apps</Out>,
              open {bot}: its address ends with the app&apos;s id, such as{" "}
              <span className="font-mono text-xs">…/manage-apps/1a2b3c4d-…</span>. Put the same id after{" "}
              <span className="font-mono text-xs">teams.microsoft.com/l/app/</span> and the link opens{" "}
              {bot}&apos;s page in Teams, with its Add button, for anyone in the organisation. Uploading
              the file again gives the app a new id. Or paste the address here:
            </p>
            <DirectLink />
          </Note>
          <p className="text-sm">
            To use it in a team&apos;s channels, add it to that team too. Open the team&apos;s{" "}
            <K>…</K> menu, choose <K>Manage team</K>, go to the <K>Apps</K> tab and press{" "}
            <K>Get more apps</K>, then find {bot} and add it.
          </p>
          <Figure caption={<>① Get more apps, on the team&apos;s Apps tab.</>}>
            <TeamAppsScreen />
          </Figure>
          <Note title="Say yes when Teams asks about channel messages">
            Adding {bot} to a team asks whether it may read that team&apos;s channel messages. With yes,
            it follows the conversation and can answer from it; without, it only hears the messages
            that mention it.
          </Note>
        </Step>

        <Step n={4} id="code" title="Send it the code" who="Whoever has the console open">
          <p className="text-sm">
            The bot doesn&apos;t answer anyone until it knows which attest_tag account it belongs to. In
            the console, press <K>Get a code</K>, then <K>Open the chat with {bot}</K>: the code is
            already typed in, so just press Send. Or type it yourself: <span className="font-mono text-xs">link ABCD-EFGH</span>{" "}
            in the chat with {bot}, or <span className="font-mono text-xs">@{bot} link ABCD-EFGH</span> in a
            channel.
          </p>
          <Figure caption={<>① The code, sent to {bot}. It answers Connected, with the name of your account.</>}>
            <LinkChatScreen />
          </Figure>
          <p className="text-sm text-muted-foreground">
            A code works once, for 30 minutes. Your Teams organisation is now one of the workspaces in
            the console, and the channels of every team {bot} is in appear under it.
          </p>
        </Step>

        <section className="space-y-4">
          <h2 className="text-lg font-semibold">Ask it something</h2>
          <p className="text-sm">
            In a channel, type <span className="font-mono text-xs">@{bot}</span> and pick it from the list,
            then ask. It answers in the same conversation. In your chat with it, just ask.
          </p>
          <Figure caption={<>① The mention. The answer comes back underneath, in the same reply chain.</>}>
            <MentionScreen />
          </Figure>
        </section>

        <section id="trouble" className="scroll-mt-6 space-y-3">
          <h2 className="text-lg font-semibold">If it does not work</h2>
          <Trouble q={<>It answers: &ldquo;I only work with … accounts&rdquo;</>}>
            The attest_tag account lets in only the email domains on its list, and it starts with the
            domain of whoever signed up. Microsoft 365 addresses are often on another domain, such as
            yourcompany.onmicrosoft.com. In the console, add it under <K>Settings → Security → Email
            domains</K>.
          </Trouble>
          <Trouble q={<>It answers: &ldquo;I&apos;m not connected to an attest_tag account in this organisation yet&rdquo;</>}>
            Send it a code (step 4).
          </Trouble>
          <Trouble q={<>It answers: &ldquo;That code didn&apos;t work&rdquo;</>}>
            Codes last 30 minutes and work once. Get a new one in the console.
          </Trouble>
          <Trouble q="It says the organisation is already connected to a different account">
            It is. Somebody who manages that account disconnects it under <K>Workspaces</K>, and then a
            new code works.
          </Trouble>
          <Trouble q={<>Teams says &ldquo;AppSideloadingForbidden&rdquo; when adding the file</>}>
            The file was added as a personal upload, which your organisation does not allow. A Teams
            administrator uploads it in the admin centre instead (step 2), and people add it from there.
          </Trouble>
          <Trouble q={<>Nobody can find {bot} in Apps, or Teams says &ldquo;This app cannot be found&rdquo;</>}>
            A new upload takes a few minutes to reach Teams: wait, then reload Teams, or open it by its
            direct link (step 3, &ldquo;Not there yet?&rdquo;). If it still is not there, your
            organisation&apos;s app policies may hide custom apps. In{" "}
            <Out href={TEAMS_MANAGE_APPS_URL}>Manage apps</Out>, search for {bot} and check it is
            allowed, and under <K>Actions → Org-wide app settings → Custom apps</K> check{" "}
            <K>Let users install and use available apps by default</K> is on.
          </Trouble>
          <Trouble q="It answers when mentioned, but not to other messages in a channel">
            It was not allowed to read the team&apos;s channel messages. Remove it from the team and add it
            again, saying yes when Teams asks.
          </Trouble>
        </section>

        <p className="border-t pt-6 text-sm text-muted-foreground">
          Using Slack instead? Slack is one button: <K>Add to Slack</K>, in the console.
        </p>
      </main>
    </div>
  );
}

// The direct link to the app in Teams, made from the address of its page in Manage apps, whose
// last part is the id the organisation's catalogue gave it. Teams opens /l/app/<that id> on the
// app's page — the manifest's own id would not do, since a custom app is known by its catalogue id.
// Nothing leaves the page: the link is put together here, from what was pasted.
const CATALOGUE_ID = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/i;

function DirectLink() {
  const [pasted, setPasted] = useState("");
  const id = CATALOGUE_ID.exec(pasted)?.[0].toLowerCase();
  const link = id ? `https://teams.microsoft.com/l/app/${id}` : "";
  return (
    <div className="mt-2 space-y-2">
      <Input
        aria-label={`The address of ${TEAMS_APP_NAME}'s page in Manage apps`}
        placeholder="https://admin.teams.microsoft.com/policies/manage-apps/…"
        value={pasted}
        onChange={(e) => setPasted(e.target.value)}
        className="font-mono text-xs"
      />
      {pasted.trim() !== "" && !link && (
        <p className="text-xs text-danger">
          There is no app id in that. Copy the whole address from the browser while {TEAMS_APP_NAME}&apos;s
          page in Manage apps is open.
        </p>
      )}
      {link && (
        <div className="flex items-center gap-1">
          <code className="min-w-0 flex-1 truncate rounded bg-muted px-2 py-1.5 font-mono text-xs text-foreground">
            {link}
          </code>
          <CopyButton text={link} label="Copy" />
          <Button asChild size="sm" variant="outline">
            <a href={link} target="_blank" rel="noreferrer">
              <ExternalLink className="size-4" /> Open
            </a>
          </Button>
        </div>
      )}
    </div>
  );
}

function Trouble({ q, children }: { q: React.ReactNode; children: React.ReactNode }) {
  return (
    <details className="group rounded-lg border bg-card px-4 py-3 text-sm">
      <summary className="cursor-pointer font-medium marker:text-muted-foreground">{q}</summary>
      <p className="mt-2 text-muted-foreground">{children}</p>
    </details>
  );
}
