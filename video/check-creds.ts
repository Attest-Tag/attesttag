/**
 * Tests the credentials in video/.env against the same calls the console's
 * connection test makes, and prints what they can reach.
 *
 * Run:  npm run check
 *
 * **Nothing here prints a token, or any part of one.** It prints who the token
 * says you are and what it says you may do, which is the question — and it is
 * the reason this script exists rather than someone reading a token out to
 * somebody else to check.
 *
 * Run it before a render. Discovering at chapter four that the PAT is scoped
 * to the wrong repository costs the take and a set of paid narration.
 */
import path from "node:path";
import { config } from "dotenv";

config({ path: path.join(process.cwd(), ".env") });

const GITHUB_TOKEN = process.env.VIDEO_GITHUB_TOKEN ?? "";
const GITHUB_REPO = process.env.VIDEO_GITHUB_REPO ?? "";
const CLICKUP_TOKEN = process.env.VIDEO_CLICKUP_TOKEN ?? "";

const ok = (m: string) => console.log(`  ✓ ${m}`);
const no = (m: string) => console.log(`  ✗ ${m}`);

/** The scopes the walkthrough actually needs, and the chapter that needs them. */
const NEEDS = {
  github: "contents:read — chapter 4 asks what changed in the repo",
  clickup: "task create — chapter 5 has it raise a ticket, and stop for a human",
};

async function checkGitHub() {
  console.log("\nGitHub  (preset: bearer · api.github.com · GET /user)");
  if (!GITHUB_TOKEN) {
    no("VIDEO_GITHUB_TOKEN is not set in video/.env");
    console.log("    Add:  VIDEO_GITHUB_TOKEN=github_pat_…");
    console.log(`    ${NEEDS.github}`);
    return;
  }

  const headers = {
    Authorization: `Bearer ${GITHUB_TOKEN}`,
    Accept: "application/vnd.github+json",
    "X-GitHub-Api-Version": "2022-11-28",
  };

  const me = await fetch("https://api.github.com/user", { headers }).catch(
    () => null,
  );
  if (!me) return no("could not reach api.github.com");
  if (!me.ok) {
    no(`GET /user → ${me.status} ${me.statusText}`);
    if (me.status === 401) {
      console.log("    The token is wrong, revoked, or expired.");
    }
    return;
  }
  const user = (await me.json()) as { login?: string; type?: string };
  ok(`authenticates as ${user.login} (${user.type})`);

  if (!GITHUB_REPO) {
    no("VIDEO_GITHUB_REPO is not set, so repo access was not checked");
    return;
  }
  const repo = await fetch(`https://api.github.com/repos/${GITHUB_REPO}`, {
    headers,
  }).catch(() => null);
  if (!repo) return no(`could not reach the repo endpoint`);
  if (!repo.ok) {
    no(`GET /repos/${GITHUB_REPO} → ${repo.status}`);
    console.log(
      repo.status === 404
        ? "    404 on a fine-grained PAT usually means NOT FOUND *or* not\n" +
          "    granted — they are the same response. Check the token's\n" +
          "    repository access list, not just the repo name."
        : "    Check the token's repository access.",
    );
    return;
  }
  const info = (await repo.json()) as {
    full_name?: string;
    private?: boolean;
    permissions?: Record<string, boolean>;
    pushed_at?: string;
  };
  ok(`can read ${info.full_name} (${info.private ? "private" : "public"})`);

  // Deliberately NOT printing repo.permissions. That field is the
  // AUTHENTICATED USER's role on the repository — it says "admin" for an owner
  // holding a read-only token — and reading it as the token's grant is how you
  // talk yourself into believing a correctly scoped token is dangerous.
  //
  // The only honest scope test is what the token can actually reach. And the
  // count from /user/repos is not it either: a fine-grained PAT can always
  // read every PUBLIC repository on GitHub, which GitHub states on the token
  // page itself, so a token scoped to one repository still lists dozens. The
  // number that matters is how many PRIVATE repositories it can see.
  const all = await fetch(`${"https://api.github.com"}/user/repos?per_page=100`, {
    headers,
  }).catch(() => null);
  if (all?.ok) {
    const repos = (await all.json()) as { full_name: string; private: boolean }[];
    const otherPrivate = repos.filter(
      (r) => r.private && r.full_name !== GITHUB_REPO,
    );
    if (otherPrivate.length === 0) {
      ok(
        `scoped: no private repo but ${GITHUB_REPO} ` +
          `(it also lists ${repos.filter((r) => !r.private).length} public ones, ` +
          `which every PAT can read and which is not a grant)`,
      );
    } else {
      no(`reaches ${otherPrivate.length} OTHER private repositories:`);
      for (const r of otherPrivate.slice(0, 8)) console.log(`      ${r.full_name}`);
      console.log("    Re-issue the token as 'Only select repositories'.");
    }
  }

  // Chapter 4 asks what changed "since Monday". An empty repo makes that
  // chapter a shot of the bot correctly reporting nothing, which is true and
  // useless.
  const commits = await fetch(
    `https://api.github.com/repos/${GITHUB_REPO}/commits?per_page=5`,
    { headers },
  ).catch(() => null);
  if (commits?.ok) {
    const list = (await commits.json()) as unknown[];
    if (list.length) ok(`${list.length}+ commits to talk about`);
    else
      no(
        "the repo has no commits — chapter 4 would be the bot correctly\n" +
          "    reporting that nothing happened. Push something first.",
      );
  }
}

async function checkClickUp() {
  console.log(
    "\nClickUp  (preset: header Authorization, no prefix · api.clickup.com · GET /api/v2/user)",
  );
  if (!CLICKUP_TOKEN) {
    no("VIDEO_CLICKUP_TOKEN is not set in video/.env");
    console.log("    Add:  VIDEO_CLICKUP_TOKEN=pk_…");
    console.log(`    ${NEEDS.clickup}`);
    return;
  }

  // No "Bearer". ClickUp takes the raw token, which is what HeaderPrefix: ""
  // in the preset means — and getting that wrong is a 401 that reads exactly
  // like a bad token.
  const headers = { Authorization: CLICKUP_TOKEN };

  const me = await fetch("https://api.clickup.com/api/v2/user", {
    headers,
  }).catch(() => null);
  if (!me) return no("could not reach api.clickup.com");
  if (!me.ok) {
    no(`GET /api/v2/user → ${me.status} ${me.statusText}`);
    if (me.status === 401) {
      console.log(
        "    Either the token is wrong, or it was sent with a 'Bearer '\n" +
          "    prefix. ClickUp wants the raw token.",
      );
    }
    return;
  }
  const user = (await me.json()) as {
    user?: { username?: string; email?: string };
  };
  const email = user.user?.email ?? "";
  ok(
    `authenticates as ${user.user?.username ?? "?"}` +
      (email ? ` <…@${email.split("@")[1]}>` : ""),
  );

  const teams = await fetch("https://api.clickup.com/api/v2/team", {
    headers,
  }).catch(() => null);
  if (!teams?.ok) return no("GET /team failed — cannot see any workspace");
  const data = (await teams.json()) as {
    teams?: { id: string; name: string; members?: unknown[] }[];
  };
  const list = data.teams ?? [];
  if (!list.length) return no("the token can see no workspaces");
  for (const team of list) {
    ok(`workspace "${team.name}" (id ${team.id}, ${team.members?.length ?? "?"} members)`);
  }
  if (list.length > 1) {
    console.log(
      "    More than one workspace. Make sure the fresh one is the only\n" +
        "    thing this token can reach, or the wrong name lands on camera.",
    );
  }
}

async function main() {
  console.log("Checking what the recording's credentials can reach.");
  console.log("(No token, or part of one, is printed.)");
  await checkGitHub();
  await checkClickUp();
  console.log(
    "\nRevoke both as soon as the recording is done. A secret that appears" +
      "\non camera has to die on camera — the video is public and permanent.",
  );
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
