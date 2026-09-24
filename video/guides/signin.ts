import type { Guide, LongForm } from "../types";
import {
  assertDb,
  assertWorkspaceState,
  dbCount,
  dbValue,
  escapeRe,
  outDir,
  partsDirFor,
  totp,
} from "./shared";

// "Sign-in, two-factor, and who gets in" — the console walkthrough.
//
// Two parts, console only, recorded in the organisation the deep dive founded:
//
//   1-users     Settings → Users: who can open the console and as what; an
//               invitation sent and withdrawn; Settings → Roles: the three
//               built-in roles and what each may do
//   2-security  Settings → Security: two-factor enrolled on the founder's own
//               account (QR, setup key, a code, the recovery codes), then the
//               organisation's policy — require two-factor for everyone — and
//               why a developer API key is exempt from it
//
// Environment (video/.env, gitignored; read when called, never at module scope):
//
//   VIDEO_INVITE_EMAIL   required by 1-users. A throwaway mailbox you control.
//                        It is plus-addressed with the minute on every take
//                        (user+signin202609121530@…): the server allows three
//                        invitations per address per day (limits.go) and a
//                        third take in one afternoon would be a 429 on camera.
//                        With RESEND_API_KEY set on the instance (.env.testing
//                        has one) the invitation is REALLY EMAILED there; on an
//                        instance with no mailer the page shows the invite link
//                        instead and the take carries on either way. The
//                        address is on screen in the pending row and in the
//                        toast; it is never narrated.
//
// What is on camera that should not outlive the recording: the two-factor
// setup key (as text and as a QR code) and the ten recovery codes. Both are
// the founder's, in the deep dive's throwaway organisation, and they die with
// it — a fresh database is a fresh founder with no second factor. Nothing here
// prints either to the log.
//
// Re-takes. Part 1 cleans up after itself: the invitation it creates is revoked
// in its last scene, and a second invitation to the same address supersedes
// the first anyway (NewEmailToken). Part 2 does NOT: two-factor stays on and
// the requirement stays on, deliberately — the brief is to leave a setting on
// that is safe to leave on. So a second take of 2-security in the same
// organisation has no "Turn on" button to press. Its `before` says so and how
// to reset: Settings → Security → Require two-factor → Optional, then
// Two-factor → Turn off (it asks for VIDEO_SIGNUP_PASSWORD) — or a fresh
// database and the deep dive again.
//
// What it leaves behind for the other walkthroughs: the founder's password
// sign-in now owes a code, and nobody holds an authenticator for it. The
// recorder's own session survives — confirming the code marks the session as
// MFA-verified (handleTOTPConfirm → MarkSessionMFA), and requireAdmin reads
// that flag — so every later walkthrough records exactly as before. A fresh
// `npm run signin` into this organisation would not. Record this one last, or
// accept a fresh database afterwards.
//
// testing.db holds one organisation, the deep dive's, and every query here
// assumes that the way shared.ts does.
//
// Every write ends with assertDb. Nothing here waits on the model; there is no
// Slack in it at all.

const partsDir = partsDirFor("signin");
const TITLE = "Sign-in, two-factor, and who gets in";

const CARD_CHAPTERS = [
  "Who can open the console",
  "Invite someone",
  "Built-in roles",
  "Withdraw the invitation",
  "Your second factor",
  "Rules for everyone",
  "A key is exempt",
];

// ---------------------------------------------------------------------------

/**
 * The address the invitation goes to, plus-addressed with the minute so every
 * take is a new recipient to the server's per-address limit. Throws when the
 * variable is unset: there is no address to default to.
 */
// shared candidate — the same stamping signupEmail() does, for the same reason
function inviteEmail(): string {
  const base = process.env.VIDEO_INVITE_EMAIL ?? "";
  const at = base.indexOf("@");
  if (at <= 0 || at === base.length - 1) {
    throw new Error(
      "1-users invites somebody to the console on camera and needs VIDEO_INVITE_EMAIL in\n" +
        "video/.env (gitignored): a throwaway mailbox you control. With RESEND_API_KEY set on\n" +
        "the instance the invitation is really sent there; without a mailer the link is shown\n" +
        "on the page instead. It is plus-addressed with the minute on every take.",
    );
  }
  const stamp = new Date().toISOString().slice(0, 16).replace(/\D/g, "");
  return `${base.slice(0, at)}+signin${stamp}${base.slice(at)}`.toLowerCase();
}

/** A literal inside SQL. */
const sql = (s: string) => s.replace(/'/g, "''");

// The founder's address must be confirmed: inviting needs users.manage, which
// waits on a verified email whenever a mailer is configured (needsVerifiedEmail
// in console_users.go). 0-signup confirms it the way the mail link would.
const FOUNDER_VERIFIED = "select count(*) from users u join orgs o on o.created_by=u.id where u.email_verified=1;";
const TOTP_ON = "select count(*) from user_totp where confirmed_at is not null;";
const REQUIRE_2FA = "select coalesce((select value from settings where key='require_two_factor' order by org_id desc limit 1), '0');";

// --- 1. users -------------------------------------------------------------------

const users: Guide = {
  id: "1-users",
  title: TITLE,
  cardChapters: CARD_CHAPTERS,
  before: async () => {
    assertWorkspaceState();
    inviteEmail(); // throws with the instructions when VIDEO_INVITE_EMAIL is unset
    if (dbCount(FOUNDER_VERIFIED) < 1) {
      throw new Error(
        "The founder's email address is unconfirmed, and inviting somebody waits on a confirmed\n" +
          "address while a mailer is configured. The deep dive's 0-signup confirms it\n" +
          "(confirmEmailInDatabase in guides/shared.ts) — run the deep dive from a fresh database.",
      );
    }
  },
  scenes: [
    {
      chapter: "Who can open the console",
      say: `Settings, then Users. Everyone who can open this console, and as what. One person so far: the account that founded the organisation, holding the admin role, which can do everything.`,
      act: async (s) => {
        await s.app.goto("/settings");
        // The tab strip: settings-page.tsx renders one TabsTrigger per TAB_LABELS entry.
        await s.app.click(s.page.getByRole("tab", { name: /^users$/i }));
        await s.wait(1_400);
        // The role cell on the founder's row. console-users-panel.tsx gives the
        // select `aria-label="Role for <name>"`; a Radix Select trigger is a combobox.
        await s.app.point(s.page.getByRole("combobox", { name: /^role for /i }).first());
        await s.wait(600);
      },
    },
    {
      say: `Nobody signs up into an organisation. Signing up founds a new one, so an invitation is the only way in, and the role it carries decides what they see.`,
      act: async (s) => {
        await s.app.scroll(320);
        await s.app.point(s.page.getByText(/^invite someone$/i).first());
        await s.wait(500);
      },
    },
    {
      chapter: "Invite someone",
      say: `Invite someone by email address, or by Slack user id, and the bot sends the link as a direct message. The role is viewer, the default: it reads what exists and changes nothing.`,
      act: async (s, ctx) => {
        ctx.inviteEmail = inviteEmail();
        // InvitePersonForm in console-users-panel.tsx: the one field takes either
        // an address or a U… id and says which it took underneath.
        await s.app.typeVerified('input[aria-label="Who to invite"]', ctx.inviteEmail, 34);
        await s.wait(400);
        await s.app.point(s.page.getByRole("combobox", { name: /^role for the invite$/i }));
        await s.wait(500);
      },
    },
    {
      say: `Send it. The invitation is single-use, expires in seven days, and only that address can redeem it. It waits here until it is accepted.`,
      act: async (s, ctx) => {
        // Exact: "Revoke the invite for …" is also a button on this page.
        await s.app.click(s.page.getByRole("button", { name: /^invite$/i }));
        // Done is the pending row for OUR address (PendingRow's revoke button
        // is labelled with it), not a toast: the list is re-read from the
        // server after the create, so the row is proof the write landed. The
        // mail send can take a while when the provider is slow, hence 30s.
        await s.page
          .getByRole("button", { name: new RegExp(`^revoke the invite for ${escapeRe(ctx.inviteEmail)}$`, "i") })
          .first()
          .waitFor({ state: "visible", timeout: 30_000 });
        await s.wait(800);
        assertDb(
          `select count(*) from email_tokens where kind='invite' and used_at is null and email='${sql(ctx.inviteEmail)}';`,
          1,
          "The invitation was not created",
        );
      },
    },
    {
      chapter: "Built-in roles",
      // A reading shot; the line is written long on purpose.
      say: `Roles. A role is a set of permissions, and everyone who can open the console holds exactly one. Three are built in: admin holds everything; editor looks after content and channels, with no credentials and no people; viewer reads what exists and changes nothing.`,
      act: async (s) => {
        await s.app.click(s.page.getByRole("tab", { name: /^roles$/i }));
        await s.wait(1_400);
        await s.app.scroll(160, 10);
      },
    },
    {
      say: `Custom roles are yours to define, from the permissions you hold yourself. A role can never hand out more than its maker has.`,
      act: async (s) => {
        // console-roles-panel.tsx shows "New role" to anyone with roles.manage —
        // the founder is an admin, so it is there.
        await s.app.point(s.page.getByRole("button", { name: /^new role$/i }));
        await s.wait(600);
      },
    },
    {
      chapter: "Withdraw the invitation",
      say: `Back on Users. The invitation has not been accepted yet, so withdraw it. The link stops working the moment it is revoked, and nothing else changes.`,
      act: async (s, ctx) => {
        await s.app.click(s.page.getByRole("tab", { name: /^users$/i }));
        await s.wait(1_200);
        // An emailed invitation revokes without a confirm dialog (only a share
        // link asks); the row leaves the list once the server has answered.
        const revoke = s.page
          .getByRole("button", { name: new RegExp(`^revoke the invite for ${escapeRe(ctx.inviteEmail)}$`, "i") })
          .first();
        await s.app.click(revoke, { settle: 800 });
        await revoke.waitFor({ state: "hidden", timeout: 15_000 });
        await s.wait(600);
        assertDb(
          `select count(*) from email_tokens where kind='invite' and used_at is not null and email='${sql(ctx.inviteEmail)}';`,
          1,
          "The invitation was not revoked",
        );
      },
    },
  ],
};

// --- 2. security ----------------------------------------------------------------

const security: Guide = {
  id: "2-security",
  title: TITLE,
  before: async () => {
    assertWorkspaceState();
    // Both checks exist because a re-take cannot enrol twice, and the switch
    // scene would turn an already-on requirement OFF on camera while the line
    // says it is being turned on.
    if (dbCount(TOTP_ON) > 0) {
      throw new Error(
        "Two-factor is already on for the deep dive's account, so the Security tab has no\n" +
          "\"Turn on\" to press. Reset it first, in the console: Settings → Security → Require\n" +
          "two-factor → Optional, then Two-factor → Turn off (it asks for VIDEO_SIGNUP_PASSWORD).\n" +
          "Or start again: FRESH_DB=1 ./start_dev.sh, then npm run video:deep.",
      );
    }
    if (dbValue(REQUIRE_2FA) === "1") {
      throw new Error(
        "require_two_factor is already on for this organisation, and this part turns it on.\n" +
          "Set it to Optional first: Settings → Security → Require two-factor.",
      );
    }
  },
  scenes: [
    {
      chapter: "Your second factor",
      say: `Security. Your own sign-in first: a password, a second factor, and a Slack account. Below it, the rules for everyone in this organisation.`,
      act: async (s) => {
        await s.app.goto("/settings");
        await s.app.click(s.page.getByRole("tab", { name: /^security$/i }));
        await s.wait(1_400);
      },
    },
    {
      say: `Turn two-factor on. A QR code for an authenticator app, and the same key as text beside it, for typing in by hand. It is shown once.`,
      act: async (s, ctx) => {
        // security-panel.tsx TwoFactorField: "Turn on" while the factor is off.
        await s.app.click(s.page.getByRole("button", { name: /^turn on$/i }), { settle: 800 });
        // The setup key is the <code> beside the "Copy the setup key" button
        // (two-factor-enrolment.tsx); scoped to that button's own row so no
        // other <code> on the page can be read by mistake. The recorder reads
        // it the way a person would type it into an app.
        const keyEl = s.page.locator('div:has(> button[aria-label="Copy the setup key"]) > code').first();
        await keyEl.waitFor({ state: "visible", timeout: 20_000 });
        const key = ((await keyEl.textContent()) ?? "").trim();
        // Base32, unpadded, as totp.go issues it. Anything else means the
        // selector found the wrong element, and typing a code from it would
        // fail three scenes later with a message about the phone's clock.
        if (!/^[A-Z2-7]{16,}$/i.test(key)) {
          throw new Error(`The setup key on the page does not read as a base32 secret (${key.length} chars).`);
        }
        ctx.totpKey = key;
        await s.wait(1_000);
      },
    },
    {
      say: `Type the six digits the app shows. The server derives the same digits from the same key, allows one thirty-second step of drift either side, and spends the step it matched. That code will not work twice.`,
      act: async (s, ctx) => {
        // Computed at the last moment: a code is good for its own 30-second
        // step and one either side (totpSkew in totp.go), which is plenty for
        // six keystrokes and a click, and not for a code computed a scene ago.
        await s.app.typeVerified("#totp-code", totp(ctx.totpKey), 90);
        await s.wait(400);
        await s.app.click(s.page.getByRole("button", { name: /^confirm$/i }), { settle: 800 });
        // Done is the recovery codes (RecoveryCodes in two-factor-enrolment.tsx),
        // which only exist once the server accepted the code.
        await s.page
          .getByRole("button", { name: /^i have saved them$/i })
          .first()
          .waitFor({ state: "visible", timeout: 20_000 });
        await s.wait(600);
        // user_totp.confirmed_at is the column that means "on" (store_security.go).
        assertDb(TOTP_ON, 1, "Two-factor was not confirmed");
      },
    },
    {
      chapter: "Recovery codes",
      // A held shot. totp.go: ten codes, single use — SpendRecoveryCode marks
      // one used in the same statement that finds it — shown once at enrolment
      // and never again; asking for a new set replaces the lot.
      say: `Ten recovery codes, for the day the phone is gone. Each one signs you in once. This is the only time they are shown, so write them down somewhere that is not your phone.`,
      act: async () => {
        assertDb("select count(*) from user_recovery_codes where used_at is null;", 10, "The recovery codes were not issued");
      },
    },
    {
      say: `Saved. Two-factor is on, with ten unused codes. From now on, a password sign-in on this account asks for a code after the password.`,
      act: async (s) => {
        await s.app.click(s.page.getByRole("button", { name: /^i have saved them$/i }), { settle: 800 });
        await s.wait(1_200);
      },
    },
    {
      chapter: "Rules for everyone",
      say: `Below, the policy for everyone. Which credentials open this console: a password, Slack, or either. And whether every password sign-in has to carry a code.`,
      act: async (s) => {
        await s.app.scroll(420);
        // security-panel.tsx PolicyField: the select's trigger is #auth-policy.
        // Pointed at, not changed — the one setting this part changes is below it.
        await s.app.point("#auth-policy");
        await s.wait(700);
      },
    },
    {
      say: `Require it. It saves as you change it. You could not have set this before enrolling: you may only impose what you already satisfy, so whoever turns it on is proof that at least one admin can still get in.`,
      act: async (s) => {
        // RequireTwoFactorField: a Radix switch with id require_two_factor; it
        // PUTs the setting on change and toasts "Two-factor is now required".
        // The server refuses it (409, selfLockout in account.go) unless the
        // caller's own factor is confirmed — which the scenes above made true.
        const sw = s.page.locator("#require_two_factor").first();
        await sw.waitFor({ state: "visible", timeout: 20_000 });
        if ((await sw.getAttribute("aria-checked")) === "true") {
          throw new Error("Require two-factor is already on; clicking it now would turn it off on camera.");
        }
        await s.app.click(sw, { settle: 1_200 });
        // The switch moves optimistically, so the toast — not the switch — is
        // the sign the save was accepted. assertDb is the proof either way.
        await s.page
          .getByText(/two-factor is now required/i)
          .first()
          .waitFor({ state: "visible", timeout: 15_000 })
          .catch(() => {});
        await s.wait(600);
        assertDb("select count(*) from settings where key='require_two_factor' and value='1';", 1, "Require two-factor was not saved");
      },
    },
    {
      say: `Your own Turn off has gone with it: nobody is the exception, including the person who set the rule. And it is checked on every request, not at sign-in. A member without a second factor is held at an enrolment screen on their next click, not their next login.`,
      act: async (s) => {
        // The save re-reads the account (onSaved → account.reload), and the
        // "Turn off" button is rendered only while the factor is optional —
        // so the row now shows "New recovery codes" alone.
        await s.app.scroll(-420);
        await s.app.point(s.page.getByRole("button", { name: /^new recovery codes$/i }));
        await s.wait(1_800);
        await s.app.scroll(420);
        // The requirement's own description on the page says where a member
        // is held (security-panel.tsx); requireAdmin in auth_slack.go is
        // where it is enforced, on every authenticated request.
        await s.app.point(s.page.getByText(/held at an enrolment screen/i).first());
        await s.wait(600);
      },
    },
    {
      chapter: "A key is exempt",
      // From api_keys.go: a key carries its maker's membership, role and
      // permissions, re-resolved per request, but not the sign-in policy or
      // the two-factor requirement — "a nightly job has no keyboard". What
      // bounds it is its own expiry, checked on every request, and Revoke.
      say: `One thing is exempt on purpose: a developer API key. It carries its maker's access and nothing more, but not the sign-in policy and not the two-factor rule. A nightly job has no keyboard and cannot be shown an enrolment screen. What bounds a key instead is its own expiry, checked on every request, and the revoke button.`,
      act: async (s) => {
        // Empty state or a table with an Expires column, depending on whether
        // the api walkthrough has run; both say what the line says.
        await s.app.goto("/developer/api-keys");
        await s.wait(1_400);
        await s.app.scroll(160, 10);
      },
    },
    {
      say: `A password, a code, and a policy. Each is checked on every request, for everyone, including the person who set it.`,
      act: async (s) => {
        // settings-page.tsx reads ?tab= after mount, so this lands on the tab directly.
        await s.app.goto("/settings?tab=security");
        await s.wait(1_400);
      },
    },
  ],
};

export const SIGNIN: LongForm = {
  id: "signin",
  title: TITLE,
  cardChapters: CARD_CHAPTERS,
  partsDir,
  outDir,
  parts: [users, security],
  // The enrolment card: a QR code and a setup key beside a six-digit field —
  // the one frame in this walkthrough where the product is visibly doing the
  // thing rather than listing settings. The secret it encodes is the throwaway
  // founder's and is in the video anyway.
  poster: { chapter: "Your second factor", offset: 15 },
};
