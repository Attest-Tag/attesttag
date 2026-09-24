/**
 * Fills the demo GitHub repository and ClickUp workspace with the week the
 * Slack backstory is about.
 *
 * Run:  npm run seed              both
 *       npm run seed -- github
 *       npm run seed -- clickup
 *
 * **This writes to real services with the tokens in video/.env.** It is a
 * separate script from the recorder for that reason.
 *
 * ## Why this is not optional
 *
 * Chapter 4 asks the bot what changed in the repository since Monday, and
 * chapter 5 has it raise a ticket about work the channel discussed. Against an
 * empty repository and an empty tracker, both chapters are a shot of the bot
 * correctly reporting that there is nothing there — true, and useless.
 *
 * So the three surfaces have to tell ONE story. Slack says the retry budget
 * was argued about and revised; the repository has the commits that argument
 * produced; the tracker has the tickets around it, and is missing exactly the
 * one the channel said it needed. That missing ticket is what chapter 5 asks
 * for, which is why it is left out here on purpose.
 *
 * Safe to run twice: it skips anything already there.
 */
import path from "node:path";
import { config } from "dotenv";

config({ path: path.join(process.cwd(), ".env") });

const GITHUB_TOKEN = process.env.VIDEO_GITHUB_TOKEN ?? "";
const GITHUB_REPO = process.env.VIDEO_GITHUB_REPO ?? "";
const CLICKUP_TOKEN = process.env.VIDEO_CLICKUP_TOKEN ?? "";

/** Named, because both names are on camera in chapter 5. */
const SPACE_NAME = "Engineering";
const LIST_NAME = "Checkout";

const GH = "https://api.github.com";
const CU = "https://api.clickup.com/api/v2";

const ghHeaders = () => ({
  Authorization: `Bearer ${GITHUB_TOKEN}`,
  Accept: "application/vnd.github+json",
  "X-GitHub-Api-Version": "2022-11-28",
  "Content-Type": "application/json",
});
const cuHeaders = () => ({
  // No "Bearer" — the preset's HeaderPrefix is empty and ClickUp wants the
  // raw token. Sending a prefix is a 401 that reads like a bad token.
  Authorization: CLICKUP_TOKEN,
  "Content-Type": "application/json",
});

async function api<T>(
  url: string,
  init: RequestInit,
  what: string,
): Promise<T> {
  const res = await fetch(url, init);
  if (!res.ok) {
    const body = await res.text().catch(() => "");
    throw new Error(`${what} → ${res.status} ${res.statusText}\n${body.slice(0, 400)}`);
  }
  return (await res.json()) as T;
}

/** `n` days ago, at a plausible working hour, as an ISO string. */
function daysAgo(n: number, hour = 10): string {
  const d = new Date();
  d.setDate(d.getDate() - n);
  d.setHours(hour, (n * 17) % 60, 0, 0);
  return d.toISOString();
}

// ---------------------------------------------------------------- GitHub ---

/**
 * The commits, oldest first. Each one is a whole tree — the files as they
 * stand at that commit — which is simpler than computing diffs and is what the
 * Git Data API wants anyway.
 *
 * Written with the Git Data API rather than the Contents API for one reason:
 * the Contents API stamps every commit `now`, and "what changed since Monday"
 * is not a question you can ask a repository whose history is four seconds
 * long. Here the author date is ours to set.
 */
const COMMITS: { message: string; daysAgo: number; files: Record<string, string> }[] = [
  {
    message: "checkout: service skeleton",
    daysAgo: 9,
    files: {
      "README.md":
        "# checkout-service\n\nTakes an order, authorises payment, and writes the result.\n\n" +
        "Sample repository. Nothing here is a real service.\n",
      "internal/checkout/handler.go":
        "package checkout\n\n// Handle authorises the order and returns once the payment\n" +
        "// provider has answered, or the request budget is spent.\nfunc Handle(order Order) error {\n" +
        "\treturn payments.Authorise(order.Total, order.Method)\n}\n",
    },
  },
  {
    message: "payments: client with a fixed retry count of 2",
    daysAgo: 7,
    files: {
      "internal/payments/client.go":
        "package payments\n\n// maxAttempts is a plain count. Two felt safe.\nconst maxAttempts = 2\n\n" +
        "func Authorise(total int64, method string) error {\n\tvar err error\n" +
        "\tfor i := 0; i < maxAttempts; i++ {\n\t\tif err = call(total, method); err == nil {\n" +
        "\t\t\treturn nil\n\t\t}\n\t}\n\treturn err\n}\n",
    },
  },
  {
    message: "checkout: record timeouts per path, not just per service",
    daysAgo: 4,
    files: {
      "internal/checkout/metrics.go":
        "package checkout\n\n// Timeouts were only counted per service, which said the service\n" +
        "// was fine while one path inside it was not. Count per path.\n" +
        "var timeouts = metrics.NewCounterVec(\"checkout_timeouts_total\", \"path\")\n",
    },
  },
  {
    message: "payments: replace the retry count with a total time budget\n\n" +
      "A count is unbounded in time, which is the thing the caller\n" +
      "actually cares about. 2.5s per request, however many attempts\n" +
      "fit inside it.",
    daysAgo: 2,
    files: {
      "internal/payments/client.go":
        "package payments\n\nimport \"time\"\n\n// budget is the total time one authorisation may spend,\n" +
        "// retries included. Replaces maxAttempts: a count says nothing\n" +
        "// about how long the caller waits.\nconst budget = 2500 * time.Millisecond\n\n" +
        "func Authorise(total int64, method string) error {\n\tdeadline := time.Now().Add(budget)\n" +
        "\tvar err error\n\tfor time.Now().Before(deadline) {\n\t\tif err = call(total, method); err == nil {\n" +
        "\t\t\treturn nil\n\t\t}\n\t\ttime.Sleep(backoff())\n\t}\n\treturn err\n}\n",
    },
  },
  {
    message: "payments: jittered backoff between attempts",
    daysAgo: 1,
    files: {
      "internal/payments/backoff.go":
        "package payments\n\n// Jitter, so a downstream that is already struggling does not\n" +
        "// get every caller's retry at the same instant.\nfunc backoff() time.Duration {\n" +
        "\tbase := 100 * time.Millisecond\n\treturn base + time.Duration(rand.Int63n(int64(base)))\n}\n",
    },
  },
];

/** The branch left open and unreviewed — chapter 4 asks about exactly this. */
const OPEN_PR = {
  branch: "reconciliation-queue",
  title: "reconciliation: queue charges whose budget was exhausted",
  body:
    "Follow-up to the retry budget change. When the budget runs out we currently\n" +
    "fail closed, which turns a slow provider into a lost order.\n\n" +
    "This queues the charge instead and reconciles it later.\n\n" +
    "**Not ready** — the reconciliation side needs a matching change and nobody\n" +
    "has picked that up yet.",
  files: {
    "internal/payments/queue.go":
      "package payments\n\n// Enqueue parks an authorisation whose budget ran out, for the\n" +
      "// reconciler to retry out of band. A delayed charge is recoverable;\n" +
      "// a dropped one is a support ticket.\nfunc Enqueue(total int64, method string) error {\n" +
      "\treturn queue.Publish(\"payments.retry\", total, method)\n}\n",
  },
};

type Ref = { object: { sha: string } };

async function ghBlob(content: string): Promise<string> {
  const blob = await api<{ sha: string }>(
    `${GH}/repos/${GITHUB_REPO}/git/blobs`,
    {
      method: "POST",
      headers: ghHeaders(),
      body: JSON.stringify({ content, encoding: "utf-8" }),
    },
    "create blob",
  );
  return blob.sha;
}

async function ghCommit(
  files: Record<string, string>,
  message: string,
  date: string,
  parents: string[],
  baseTree?: string,
): Promise<string> {
  const tree = await Promise.all(
    Object.entries(files).map(async ([pathname, content]) => ({
      path: pathname,
      mode: "100644" as const,
      type: "blob" as const,
      sha: await ghBlob(content),
    })),
  );
  const created = await api<{ sha: string }>(
    `${GH}/repos/${GITHUB_REPO}/git/trees`,
    {
      method: "POST",
      headers: ghHeaders(),
      body: JSON.stringify({ tree, ...(baseTree ? { base_tree: baseTree } : {}) }),
    },
    "create tree",
  );
  const commit = await api<{ sha: string }>(
    `${GH}/repos/${GITHUB_REPO}/git/commits`,
    {
      method: "POST",
      headers: ghHeaders(),
      body: JSON.stringify({
        message,
        tree: created.sha,
        parents,
        // Both, or GitHub stamps the committer date `now` and the repository
        // sorts by it in some views.
        author: { name: "Sam Reyes", email: "sam@example.com", date },
        committer: { name: "Sam Reyes", email: "sam@example.com", date },
      }),
    },
    "create commit",
  );
  return commit.sha;
}

async function seedGitHub() {
  console.log(`\nGitHub — ${GITHUB_REPO}`);
  if (!GITHUB_TOKEN || !GITHUB_REPO) {
    console.log("  ✗ VIDEO_GITHUB_TOKEN / VIDEO_GITHUB_REPO not set");
    return;
  }

  const head = await fetch(`${GH}/repos/${GITHUB_REPO}/git/ref/heads/main`, {
    headers: ghHeaders(),
  });
  if (head.ok) {
    const ref = (await head.json()) as Ref;
    console.log(`  – main already exists at ${ref.object.sha.slice(0, 7)}; skipping commits.`);
  } else {
    // A repository with no commits at all refuses the Git Data API entirely —
    // `POST /git/blobs` answers 409 "Git Repository is empty", because there
    // is no ref for a blob to be reachable from yet. The Contents API does
    // work on one, and creates the branch as a side effect, so the first
    // commit goes through that and everything after it stacks on top.
    //
    // It takes author/committer dates too, which is the only reason this
    // bootstrap does not leave the repository's history starting today.
    const first = await api<{ commit: { sha: string } }>(
      `${GH}/repos/${GITHUB_REPO}/contents/README.md`,
      {
        method: "PUT",
        headers: ghHeaders(),
        body: JSON.stringify({
          message: "initial commit",
          content: Buffer.from(
            "# checkout-service\n\nSample repository. Nothing here is a real service.\n",
            "utf8",
          ).toString("base64"),
          branch: "main",
          author: { name: "Sam Reyes", email: "sam@example.com", date: daysAgo(10) },
          committer: { name: "Sam Reyes", email: "sam@example.com", date: daysAgo(10) },
        }),
      },
      "bootstrap first commit",
    );
    console.log("  ✓ initial commit  (10d ago)");

    const bootstrapped = await api<{ tree: { sha: string } }>(
      `${GH}/repos/${GITHUB_REPO}/git/commits/${first.commit.sha}`,
      { headers: ghHeaders() },
      "read bootstrap commit",
    );
    let parent: string | undefined = first.commit.sha;
    let tree: string | undefined = bootstrapped.tree.sha;
    for (const c of COMMITS) {
      const sha: string = await ghCommit(
        c.files,
        c.message,
        daysAgo(c.daysAgo),
        parent ? [parent] : [],
        tree,
      );
      // Read the commit back for its tree, so the next one stacks on it
      // instead of replacing the whole repository with two files.
      const full = await api<{ tree: { sha: string } }>(
        `${GH}/repos/${GITHUB_REPO}/git/commits/${sha}`,
        { headers: ghHeaders() },
        "read commit",
      );
      tree = full.tree.sha;
      parent = sha;
      console.log(`  ✓ ${c.message.split("\n")[0]}  (${c.daysAgo}d ago)`);
    }
    // The bootstrap already created the branch, so this moves it rather than
    // creating it.
    await api(
      `${GH}/repos/${GITHUB_REPO}/git/refs/heads/main`,
      {
        method: "PATCH",
        headers: ghHeaders(),
        body: JSON.stringify({ sha: parent, force: true }),
      },
      "move main",
    );
    console.log(`  ✓ refs/heads/main → ${parent!.slice(0, 7)}`);
  }

  // The open, unreviewed pull request.
  const prs = await api<{ title: string }[]>(
    `${GH}/repos/${GITHUB_REPO}/pulls?state=open`,
    { headers: ghHeaders() },
    "list pulls",
  );
  if (prs.some((p) => p.title === OPEN_PR.title)) {
    console.log("  – the open PR is already there.");
    return;
  }
  const main = await api<Ref>(
    `${GH}/repos/${GITHUB_REPO}/git/ref/heads/main`,
    { headers: ghHeaders() },
    "read main",
  );
  const mainCommit = await api<{ tree: { sha: string } }>(
    `${GH}/repos/${GITHUB_REPO}/git/commits/${main.object.sha}`,
    { headers: ghHeaders() },
    "read main commit",
  );
  const branchSha = await ghCommit(
    OPEN_PR.files,
    "reconciliation: queue exhausted charges",
    daysAgo(1, 16),
    [main.object.sha],
    mainCommit.tree.sha,
  );
  await api(
    `${GH}/repos/${GITHUB_REPO}/git/refs`,
    {
      method: "POST",
      headers: ghHeaders(),
      body: JSON.stringify({ ref: `refs/heads/${OPEN_PR.branch}`, sha: branchSha }),
    },
    "create branch",
  ).catch(() => console.log("  – branch already existed"));
  await api(
    `${GH}/repos/${GITHUB_REPO}/pulls`,
    {
      method: "POST",
      headers: ghHeaders(),
      body: JSON.stringify({
        title: OPEN_PR.title,
        head: OPEN_PR.branch,
        base: "main",
        body: OPEN_PR.body,
        draft: false,
      }),
    },
    "open pull request",
  );
  console.log(`  ✓ open PR: ${OPEN_PR.title}`);
}

// --------------------------------------------------------------- ClickUp ---

/**
 * The tracker, as it stands before the walkthrough starts.
 *
 * Note what is NOT here: a ticket for the reconciliation change. The Slack
 * backstory says it is needed and that nobody raised it, and chapter 5 is the
 * bot offering to. Seeding it would delete the chapter.
 */
const TASKS: { name: string; description: string; status?: string }[] = [
  {
    name: "Checkout timeouts on the payments path",
    description:
      "0.4% of checkout calls timed out over the weekend, all on the payments path.\n" +
      "About 900 requests. Small, but at the worst possible moment.",
  },
  {
    name: "Replace the payments retry count with a time budget",
    description:
      "A fixed count of 2 says nothing about how long the caller waits.\n" +
      "Agreed in #attest-demo: 2.5s total budget per request, jittered backoff,\n" +
      "drop the fixed count.",
  },
  {
    name: "Count checkout timeouts per path",
    description:
      "Per-service counters said the service was healthy while one path inside\n" +
      "it was not.",
  },
  {
    name: "Write the runbook entry for the retry budget change",
    description: "Before it ships on Tuesday. Deploys are Tuesdays only.",
  },
];

async function seedClickUp() {
  console.log("\nClickUp");
  if (!CLICKUP_TOKEN) {
    console.log("  ✗ VIDEO_CLICKUP_TOKEN not set");
    return;
  }

  const teams = await api<{ teams: { id: string; name: string }[] }>(
    `${CU}/team`,
    { headers: cuHeaders() },
    "list workspaces",
  );
  const team = teams.teams[0];
  if (!team) throw new Error("The ClickUp token can see no workspace.");
  console.log(`  workspace: ${team.name} (${team.id})`);

  const spaces = await api<{ spaces: { id: string; name: string }[] }>(
    `${CU}/team/${team.id}/space`,
    { headers: cuHeaders() },
    "list spaces",
  );
  // Deliberately NOT `?? spaces[0]`. A fresh ClickUp comes with "Team Space"
  // and "Project 1", and falling back to those puts the words "Project 1" on
  // camera — which reads as an unconfigured demo rather than as a team's
  // tracker, and undoes the work the rest of this file does.
  let space = spaces.spaces.find((s) => s.name === SPACE_NAME);
  if (!space) {
    space = await api<{ id: string; name: string }>(
      `${CU}/team/${team.id}/space`,
      {
        method: "POST",
        headers: cuHeaders(),
        body: JSON.stringify({ name: SPACE_NAME, multiple_assignees: false }),
      },
      "create space",
    );
    console.log(`  ✓ created space ${space.name}`);
  } else {
    console.log(`  space: ${space.name}`);
  }

  // A folderless list is the shortest path to somewhere tasks can live, and
  // the preset's own notes say to get a list id in one call rather than
  // walking /space and /folder.
  const lists = await api<{ lists: { id: string; name: string }[] }>(
    `${CU}/space/${space.id}/list`,
    { headers: cuHeaders() },
    "list lists",
  );
  let list = lists.lists.find((l) => l.name === LIST_NAME);
  if (!list) {
    list = await api<{ id: string; name: string }>(
      `${CU}/space/${space.id}/list`,
      {
        method: "POST",
        headers: cuHeaders(),
        body: JSON.stringify({ name: LIST_NAME }),
      },
      "create list",
    );
    console.log(`  ✓ created list ${list.name}`);
  } else {
    console.log(`  list: ${list.name} (${list.id})`);
  }

  const existing = await api<{ tasks: { name: string }[] }>(
    `${CU}/list/${list.id}/task`,
    { headers: cuHeaders() },
    "list tasks",
  );
  const have = new Set(existing.tasks.map((t) => t.name));

  for (const task of TASKS) {
    if (have.has(task.name)) {
      console.log(`  – ${task.name}`);
      continue;
    }
    await api(
      `${CU}/list/${list.id}/task`,
      {
        method: "POST",
        headers: cuHeaders(),
        body: JSON.stringify({ name: task.name, description: task.description }),
      },
      "create task",
    );
    console.log(`  ✓ ${task.name}`);
  }
  console.log(
    "\n  No ticket for the reconciliation change, on purpose — chapter 5 is\n" +
      "  the bot offering to raise it, and seeding it would delete the chapter.",
  );
}

async function main() {
  const wanted = process.argv.slice(2);
  const all = !wanted.length;
  if (all || wanted.includes("github")) await seedGitHub();
  if (all || wanted.includes("clickup")) await seedClickUp();
}

main().catch((err) => {
  console.error(`\n${err.message ?? err}`);
  process.exit(1);
});
