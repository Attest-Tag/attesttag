import { existsSync, readFileSync } from "node:fs";
import path from "node:path";
import type { Guide, LongForm, Surfaces } from "../types";
import {
  BUNDLE,
  assertDb,
  assertDeepDiveState,
  bot,
  channel,
  dbCount,
  commandCut,
  dbValue,
  outDir,
  partsDirFor,
  presetRow,
  waitForDb,
} from "./shared";

// "Documents and Drive" — the walkthrough.
//
// Recorded in three parts and stitched:
//
//   1-upload   a folder, a runbook uploaded into it, the scope it lands with, the index
//   2-ask      Slack answers from it; `!docs` reports the index
//   3-drive    a Google Drive credential in the bundle, a folder followed, its files mirrored
//
// It records in the organisation the deep dive founded (guides/deep-dive.ts),
// so every part checks for that state in `before` and names the run that is
// missing. The channel's seeded week argued the retry budget out; the runbook
// in fixtures/documents/ is the entry that conversation promised to write, so
// the question in part 2 has an answer the index actually holds.
//
// Environment (video/.env, gitignored), read WHEN CALLED — see LEARNINGS.md
// ("Environment, and where it is read"). Only part 3 needs these two; parts
// 1 and 2 run without them, and the check is in part 3's `before`.
//
//   VIDEO_GDRIVE_SA_JSON   Path to a service-account key file — the JSON Google
//                          Cloud downloads — from a THROWAWAY GCP project with the
//                          Drive API enabled. Part 3 pastes it into the console on
//                          camera, and the field is a plain textarea rather than a
//                          password field, so the file is legible in the frame (the
//                          box is scrolled back to its first lines; the private key
//                          sits further down). Delete the key in Google Cloud when
//                          the recording is done: secrets that appear on camera die
//                          on camera, like the deep dive's tokens.
//   VIDEO_GDRIVE_FOLDER    A Drive folder link (https://drive.google.com/drive/folders/…)
//                          that has been SHARED with that service account's
//                          client_email — Viewer is enough — and holds two or three
//                          harmless files of a type the index reads: .md, .txt, .pdf,
//                          .csv, .json, or Google Docs, which export as Markdown. A
//                          folder shared with the domain but not with that exact
//                          address previews as empty, and the take stops there.
//
// What a take leaves behind, and re-takes:
//   1-upload is safe to record again. The folder is "insert … on conflict do
//   nothing" (store_admin.go AddDocumentFolder) and the upload overwrites the
//   same path (documents_path is unique), so nothing doubles up on camera.
//   2-ask posts into the channel; `npm run tidy` removes the recorder's rows.
//   3-drive recorded twice leaves a second Google Drive credential in the bundle
//   and a second followed folder, both in the frame. Before recording it again,
//   delete the credential (Bundles → Engineering → the row's menu → Delete) and
//   stop following the folder (Documents → the folder's menu → Stop following).
//   The same folder on the same credential is refused outright (drive_syncs_folder
//   is unique), which would stop the take at the Add folder click.
//
// Two rules this file obeys, from deep-dive.ts: a scene holds for as long as
// its line takes to speak, and nothing asserts on what the model said.

const partsDir = partsDirFor("documents");
const TITLE = "Documents and Drive";

const FOLDER = "runbooks";
const FIXTURE = "payments-retry-runbook.md";
/** The document's path as the index, the API and the console all name it. */
const DOC_PATH = `${FOLDER}/${FIXTURE}`;
const fixturePath = () => path.join(process.cwd(), "fixtures", "documents", FIXTURE);

const gdriveSaJson = () => process.env.VIDEO_GDRIVE_SA_JSON ?? "";
const gdriveFolder = () => process.env.VIDEO_GDRIVE_FOLDER ?? "";

// ---------------------------------------------------------------------------
// Helpers local to this walkthrough.

/** The runbook's row in the documents table, once its folder is open. */
const docRow = (s: Surfaces) => s.page.getByRole("row").filter({ hasText: FIXTURE }).first();

/**
 * Open the runbooks folder from the top of Documents. A folder row is a
 * BUTTON carrying the folder's name (documents-page.tsx), like the workspace
 * rail entries the deep dive learned the hard way about.
 */
async function openFolder(s: Surfaces): Promise<void> {
  await s.app.click(s.page.getByRole("button", { name: new RegExp(`^${FOLDER}$`, "i") }).first(), {
    settle: 1_200,
  });
}

/** The Google Drive section on the Documents page (drive-sync-panel.tsx). */
const drivePanel = (s: Surfaces) => s.page.locator("section").filter({ hasText: /google drive/i }).first();

/**
 * The service-account key file, read and checked enough to name the address
 * a folder has to be shared with. Throws with what to set, so a missing or
 * wrong file stops the take before a line is synthesised.
 */
function serviceAccount(): { json: string; email: string } {
  const file = gdriveSaJson();
  if (!file || !gdriveFolder()) {
    throw new Error(
      "3-drive needs two things in video/.env:\n" +
        "  VIDEO_GDRIVE_SA_JSON   path to a service-account key file from a throwaway GCP project\n" +
        "                         (Drive API enabled)\n" +
        "  VIDEO_GDRIVE_FOLDER    a Drive folder link that is SHARED with that service account's\n" +
        "                         client_email (Viewer) and holds two or three harmless files\n" +
        "Parts 1 and 2 run without them; only this part reaches Drive.",
    );
  }
  if (!existsSync(file)) throw new Error(`VIDEO_GDRIVE_SA_JSON points at ${file}, which does not exist.`);
  const json = readFileSync(file, "utf8").trim();
  let parsed: { type?: string; client_email?: string };
  try {
    parsed = JSON.parse(json);
  } catch {
    throw new Error(`${file} is not JSON — it should be the key file Google Cloud downloads for a service account.`);
  }
  if (parsed.type !== "service_account" || !parsed.client_email) {
    throw new Error(`${file} is not a service-account key: expected type "service_account" and a client_email.`);
  }
  return { json, email: parsed.client_email };
}

// --- 1. upload -----------------------------------------------------------------

const upload: Guide = {
  id: "1-upload",
  title: TITLE,
  before: async () => {
    assertDeepDiveState();
    if (!existsSync(fixturePath())) {
      throw new Error(`The fixture is missing: ${fixturePath()} — it is checked in under video/fixtures/documents/.`);
    }
  },
  scenes: [
    {
      chapter: "Documents",
      say: `Documents are what it searches when it answers: runbooks, policies, the file people keep asking about. They live here, in the console.`,
      act: async (s) => {
        await s.app.goto("/documents");
        await s.wait(1_200);
      },
    },
    {
      say: `Folders are for your own ordering; it searches every document either way. One for runbooks.`,
      act: async (s) => {
        await s.app.click(s.page.getByRole("button", { name: /^new folder$/i }).first());
        // new-folder-dialog.tsx: #folder-name, and a submit reading "Create folder".
        await s.page.locator("#folder-name").waitFor({ state: "visible", timeout: 15_000 });
        await s.app.typeVerified("#folder-name", FOLDER);
        await s.app.click(s.page.getByRole("dialog").getByRole("button", { name: /^create folder$/i }));
        await s.wait(1_400);
        assertDb(`select count(*) from document_folders where path='${FOLDER}';`, 1, "The runbooks folder was not created");
        await openFolder(s);
      },
    },
    {
      chapter: "Upload a runbook",
      say: `Upload takes files, or a whole folder, or a drop anywhere on the page. The runbook for the retry path goes in, and indexing starts on its own.`,
      act: async (s) => {
        // The Upload button opens a menu (Files…, Folder…), and either item
        // opens the browser's native file dialog, which the recorder cannot
        // drive. Show the menu, close it, and hand the file to the hidden
        // input directly: documents-page.tsx renders two, and the first —
        // the one with `accept` — is the file picker; the second, with
        // webkitdirectory, is the folder picker. The empty state repeats the
        // Upload button, so the header's is `.first()`.
        await s.app.click(s.page.getByRole("button", { name: /^upload$/i }).first(), { settle: 900 });
        await s.page.keyboard.press("Escape");
        await s.wait(400);
        await s.page.locator('input[type="file"][accept]').first().setInputFiles(fixturePath());
        await docRow(s).waitFor({ state: "visible", timeout: 30_000 });
        await s.wait(800);
        assertDb(`select count(*) from documents where path='${DOC_PATH}';`, 1, "The runbook was not uploaded");
      },
    },
    {
      chapter: "Scope",
      say: `Every upload lands with a scope: the whole workspace, or one channel. A document scoped to a channel is invisible from every other channel; the search never returns it there.`,
      act: async (s) => {
        // document-scope-select.tsx: the header picker is a combobox labelled
        // "Scope for new uploads" — workspace-wide, or one channel by name.
        // Workspace-wide is the default and stays; rag.go Search drops any
        // chunk whose document is scoped to a channel other than the asker's.
        await s.app.click(s.page.getByRole("combobox", { name: "Scope for new uploads" }), { settle: 1_400 });
        await s.page.keyboard.press("Escape");
        await s.wait(600);
        // Every row carries its own picker afterwards.
        await s.app.point(docRow(s).getByRole("combobox").first());
      },
    },
    {
      say: `The row says what came of it: indexed, and the number of chunks it was split into. Nothing else to run.`,
      act: async (s) => {
        // Embedding runs after the upload replies (admin_api.go: go
        // b.reindex) and the page looks again four seconds later, which is
        // usually enough for a two-page file. When it is not, the rest of
        // the wait and the reload are cut, not held.
        await s.offCamera(async () => {
          await waitForDb(
            `select count(*) from documents where path='${DOC_PATH}' and status='indexed' and chunks>0;`,
            1,
            180_000,
            "the runbook was not indexed — is the embedding endpoint answering? (bot.log: ingest)",
          );
          const chip = docRow(s).getByText(/^indexed$/i).first();
          if (!(await chip.isVisible().catch(() => false))) {
            await s.app.goto("/documents");
            await openFolder(s);
          }
        });
        await s.app.point(docRow(s).getByText(/^indexed$/i).first());
        await s.wait(1_200);
      },
    },
  ],
};

// --- 2. ask --------------------------------------------------------------------

const ask: Guide = {
  id: "2-ask",
  title: TITLE,
  // The index part 1 started is what this part asks about, and a re-record
  // of this part alone needs it just the same — so the check names the part.
  before: async () => {
    assertDeepDiveState();
    await waitForDb(
      `select count(*) from doc_chunks where doc_id='local:${DOC_PATH}';`,
      1,
      120_000,
      "the runbook is not in the index — run `npm run video -- documents 1-upload` first",
    );
  },
  scenes: [
    {
      chapter: "Ask about the runbook",
      // Bot-wait scene, written to the round trip. The question names no
      // file; "runbook" is one of the words that force a document search on
      // the first round (agent.go forcedTool), so the status line reads
      // "Searching docs" rather than leaving it to the model.
      say: `Back in Slack. Ask what the runbook says; nothing names the file. Watch the status line: it is searching documents. The passages come back with the file they came from, so an answer can say which document it read and where. Then the footer, the same as any other answer: the model, the tokens, the cost.`,
      act: async (s) => {
        // Coming from a title card, Slack boots blank for seconds. Open it
        // inside the cut, so the line starts on a channel with something in it.
        await s.offCamera(async () => {
          await s.slack.openChannel(channel());
          await s.slack.closeThread();
        });
        // A model answer: done means the footer, not a status card that stopped changing.
        await s.slack.ask(`@${bot()} what does the runbook say we do when the retry budget is exhausted?`, { requireFooter: true });
      },
    },
    {
      say: `The search is a tool call like any other. The query it ran and the passages it got back are on the Activity page, beside the answer that used them.`,
      act: async (s) => {
        await s.wait(1_000);
        await s.slack.scroll(220);
      },
    },
    {
      chapter: "Index stats",
      say: `One command reports the index: how many documents, how many chunks. It never reaches the model, so there is no footer and nothing is spent.`,
      act: async (s) => {
        // commands.go: "N documents, M chunks indexed from `docs`."
        await commandCut(s, "!docs");
        await s.wait(1_500);
      },
    },
  ],
};

// --- 3. drive ------------------------------------------------------------------

const drive: Guide = {
  id: "3-drive",
  title: TITLE,
  before: async () => {
    assertDeepDiveState();
    serviceAccount(); // throws with what to set, and what to share it with
    if (dbCount(`select count(*) from document_folders where path='${FOLDER}';`) < 1) {
      throw new Error(
        "The runbooks folder is missing — run `npm run video -- documents 1-upload` first; the followed Drive folder lands in it.",
      );
    }
  },
  scenes: [
    {
      chapter: "A Drive credential",
      say: `Documents can also come from Google Drive. That starts with a credential, in the bundle this channel already has.`,
      act: async (s) => {
        await s.app.goto("/bundles");
        await s.wait(1_000);
        // bundle-card.tsx: a card's header row is the div whose direct child
        // is the name button (it carries aria-expanded); its "Configure"
        // opens the editor on the Credentials tab. Scoped to the row naming
        // the bundle, since every card has a Configure.
        await s.app.click(
          s.page
            .locator("div:has(> button[aria-expanded])")
            .filter({ hasText: BUNDLE })
            .first()
            .getByRole("button", { name: /^configure$/i }),
          { settle: 1_200 },
        );
        await s.app.clickIfPresent(s.page.getByRole("tab", { name: /^credentials$/i }));
        // bundle-edit-dialog.tsx: "Find a service" narrows two dozen rows to
        // the one that matters. Google Drive is the only preset that does.
        await s.app.typeVerified('input[placeholder="Find a service"]', "Drive");
        await s.wait(600);
        // Its row's button reads "Connect", or "Connect another" once a
        // take has been here before; presetRow matches either.
        await s.app.click(presetRow(s, /Google Drive/), { settle: 1_200 });
      },
    },
    {
      say: `A service account, not a person. Its key file is pasted once and sealed. On every call the proxy exchanges it for a token; the model never sees either.`,
      act: async (s) => {
        const { json } = serviceAccount();
        // connect-dialog.tsx: on the Recommended tab a gcp_sa credential is
        // the #rec-secret textarea — a plain one, not a password field. The
        // first characters go in at hand speed so the paste reads as a
        // paste, the rest in one move (typeVerified is for inputs; this is
        // a textarea holding a file), and the box is scrolled back to its
        // first lines, so the private key further down stays out of frame.
        const box = s.page.locator("#rec-secret").first();
        await box.waitFor({ state: "visible", timeout: 15_000 });
        await s.app.click(box, { settle: 120 });
        await box.pressSequentially(json.slice(0, 40), { delay: 24 });
        await box.fill(json);
        await s.wait(300);
        if ((await box.inputValue().catch(() => "")) !== json) {
          throw new Error("#rec-secret would not hold the key file — the field is React-controlled and was reset after it was filled.");
        }
        await box.evaluate((el) => {
          el.scrollTop = 0;
        });
        await s.wait(600);
        // One real call to Drive on the credential, before it is saved
        // (test-connection-bar.tsx: "HTTP 200", or "Failed" with the reason).
        await s.app.click(s.page.getByRole("button", { name: /test connection/i }).first());
        const verdict = s.page.getByRole("dialog").last().getByText(/HTTP \d{3}|^OK$|^Failed/).first();
        await verdict.waitFor({ state: "visible", timeout: 45_000 });
        const said = ((await verdict.textContent()) ?? "").trim();
        if (!/HTTP 2\d\d|^OK$/.test(said)) {
          throw new Error(`The Drive credential failed its test (${said}). Is the Drive API enabled in the key's project, and is the key still valid?`);
        }
        await s.wait(1_500);
      },
    },
    {
      say: `Connect. The credential joins the bundle, so every channel the bundle is attached to can reach Drive through it.`,
      act: async (s) => {
        // The footer's submit reads "Connect", like the catalogue rows
        // behind it; the dialog opened last is last in the document.
        await s.app.click(s.page.getByRole("button", { name: /^Connect$/ }).last());
        await s.wait(2_000);
        assertDb("select count(*) from connections where preset='gdrive';", 1, "The Google Drive credential was not saved");
        await s.wait(800);
      },
    },
    {
      chapter: "Follow a folder",
      say: `Now the folder itself. Documents, then Google Drive: add a folder.`,
      act: async (s) => {
        await s.app.goto("/documents");
        await s.wait(800);
        // drive-sync-panel.tsx: the section header holds "Add folder" (and,
        // once something is followed, a "Sync now" for all of it). The
        // dialog's submit reads "Add folder" too, so both are scoped.
        await s.app.click(drivePanel(s).getByRole("button", { name: /^add folder$/i }).first(), { settle: 1_000 });
        await s.page.locator("#drive-folder").waitFor({ state: "visible", timeout: 15_000 });
      },
    },
    {
      say: `The credential, and the folder's link from Drive's address bar. The dialog says the rule itself: the folder has to be shared with the connection's service account, or it comes back empty.`,
      act: async (s) => {
        // drive-sync-dialog.tsx: #drive-conn lists "bundle / credential". A
        // lone credential is preselected and a re-take's second one is not,
        // so it is picked either way.
        await s.app.click("#drive-conn", { settle: 900 });
        await s.app.click(s.page.getByRole("option", { name: /google drive/i }).first(), { settle: 700 });
        await s.app.typeVerified("#drive-folder", gdriveFolder(), 14);
        await s.app.click("#drive-dest", { settle: 900 });
        await s.app.click(s.page.getByRole("option", { name: new RegExp(`^${FOLDER}$`, "i") }).first(), { settle: 700 });
        // The scope picker has no id of its own — the label's htmlFor points
        // at nothing — so it is the combobox with this aria-label. Every
        // file the folder brings in takes it; workspace-wide stays.
        await s.app.click(s.page.getByRole("combobox", { name: "Scope for everything this folder brings in" }), { settle: 1_000 });
        await s.page.keyboard.press("Escape");
        await s.wait(400);
      },
    },
    {
      say: `Test reads the folder on that credential and lists what a pass would bring in, before anything is saved. It sees only what is shared with its own address, never everything the domain has.`,
      act: async (s) => {
        const dialog = s.page.getByRole("dialog").last();
        await s.app.click(dialog.getByRole("button", { name: /^test( again)?$/i }).first());
        // drive-preview.tsx: the listing ("3 files would come in …"), or a
        // red alert carrying Drive's reason. Polled rather than raced: a
        // waitFor that loses the race times out on its own later, and an
        // unhandled rejection is not how a take should end.
        const listed = dialog.getByText(/would come in/i).first();
        const refused = dialog.getByRole("alert").first();
        const deadline = Date.now() + 60_000;
        while (Date.now() < deadline && !(await listed.isVisible().catch(() => false))) {
          if (await refused.isVisible().catch(() => false)) {
            throw new Error(
              `Drive refused the folder: ${((await refused.textContent()) ?? "").trim()}\n` +
                `Share VIDEO_GDRIVE_FOLDER with ${serviceAccount().email} (Viewer) and record this part again.`,
            );
          }
          await s.wait(400);
        }
        if (!(await listed.isVisible().catch(() => false))) throw new Error("Test did not answer in 60s.");
        const files = Number(((await listed.textContent()) ?? "").match(/(\d+) files? would come in/)?.[1] ?? "0");
        if (files === 0) {
          throw new Error(
            `The preview lists nothing that would come in. The folder must hold files of a type the index reads ` +
              `(.md .txt .pdf .csv .json, or Google Docs) and be shared with ${serviceAccount().email}.`,
          );
        }
        await s.wait(1_800);
      },
    },
    {
      say: `Add it, then sync now rather than waiting for the six-hourly pass. Each file is copied into the runbooks folder as a document and indexed like an upload.`,
      act: async (s) => {
        const syncsBefore = dbCount("select count(*) from drive_syncs;");
        const filesBefore = dbCount("select count(*) from drive_sync_files;");
        await s.app.click(s.page.getByRole("dialog").last().getByRole("button", { name: /^add folder$/i }));
        await s.page.locator("#drive-folder").waitFor({ state: "hidden", timeout: 20_000 });
        assertDb("select count(*) from drive_syncs;", syncsBefore + 1, "The Drive folder was not added");
        await s.wait(800);
        // The row's own "Sync now"; the header's runs every folder at once.
        await s.app.click(drivePanel(s).locator("li").getByRole("button", { name: /^sync now$/i }).last(), { settle: 600 });
        // The pass runs inside the request (admin_api.go: the client waits on
        // it). Two or three files take seconds, which is the product's own
        // wait and stays on camera; anything longer is cut. A pass that
        // failed says so on the row, and here.
        const mirrored = () => dbCount("select count(*) from drive_sync_files;") > filesBefore;
        const failure = () =>
          dbValue("select case when last_status='error' then coalesce(last_error,'error') else '' end from drive_syncs order by id desc limit 1;");
        const started = Date.now();
        while (!mirrored() && !failure() && Date.now() - started < 8_000) await s.wait(500);
        if (!mirrored() && !failure()) {
          await s.offCamera(async () => {
            while (!mirrored() && !failure()) {
              if (Date.now() - started > 180_000) throw new Error("The Drive pass did not finish in 3 min.");
              await s.wait(1_000);
            }
          });
        }
        if (failure()) throw new Error(`The Drive pass failed: ${failure()}`);
        assertDb("select count(*) from drive_sync_files;", filesBefore + 1, "No file was mirrored from Drive");
        await s.wait(1_200);
      },
    },
    {
      chapter: "Copies, not links",
      say: `These are copies, not links. The text is in the index, so an answer cites it without a round trip and still works when Drive is slow. Every six hours the folder is checked again: a file that changed is refreshed, and one that left Drive leaves here too.`,
      act: async (s) => {
        await openFolder(s);
        // documents-page.tsx: a mirrored row carries the Drive mark, with
        // "Synced from Google Drive" beside the name for a screen reader.
        await s.app.point(s.page.getByRole("row").filter({ hasText: /synced from google drive/i }).first());
        await s.wait(1_500);
      },
    },
  ],
};

export const DOCUMENTS: LongForm = {
  id: "documents",
  title: TITLE,
  cardChapters: [
    "Documents",
    "Upload a runbook",
    "Scope",
    "Ask about the runbook",
    "Index stats",
    "A Drive credential",
    "Follow a folder",
    "Copies, not links",
  ],
  partsDir,
  outDir,
  parts: [upload, ask, drive],
  // The answer to the runbook question, growing in its thread: the product
  // doing the thing, in a channel with a conversation in it.
  poster: { chapter: "Ask about the runbook", offset: 14 },
};
