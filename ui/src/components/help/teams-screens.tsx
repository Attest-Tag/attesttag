import { cn } from "@/lib/utils";

// Pictures of Microsoft's screens for the Teams guide, drawn rather than captured. A screenshot
// goes stale the week Microsoft moves a button, is blurry on a large screen and unreadable on a
// small one, and carries whoever took it — their name, their organisation, their chats. A drawing
// keeps only what the step needs: the real labels, where they sit, and a numbered ring around what
// to press, with everything else reduced to the grey shapes it looks like at a glance.
//
// Inside the frame the colours are Microsoft's light theme on purpose, whatever the console's
// theme: it is a picture of their product, and it should look like what the reader will see.

const PURPLE = "#5b5fc7"; // Teams' brand purple, on its primary buttons and links

/** The app's icon as Teams shows it: the package's color.png, which is assets/logo/app-icon.svg.
 *  Fixed colours rather than the console's BrandMark, whose ink follows the console's theme and
 *  would vanish against this white in dark mode. */
function AppIcon({ size = "sm" }: { size?: "sm" | "lg" }) {
  return (
    <svg
      viewBox="0 0 256 256"
      aria-hidden
      className={cn("shrink-0 rounded-md border border-neutral-200 bg-white", size === "lg" ? "size-10" : "size-6")}
    >
      <g transform="translate(43.5 43.5) scale(0.66)" fill="none" strokeWidth={30} strokeLinejoin="round">
        <path d="M97.6 121.6 L140 172.2 L210.9 112.7" stroke="#5a50c8" />
        <path
          d="M210.9 112.7 A48 48 0 0 0 180 28 L76 28 A48 48 0 0 0 28 76 L28 180 A48 48 0 0 0 76 228 L180 228 A48 48 0 0 0 223.5 200.3"
          stroke="#191c2b"
        />
      </g>
    </svg>
  );
}

/** One numbered ring: what to press, in the order the caption lists it. */
function Hot({
  n,
  children,
  className,
  badge = "right",
}: {
  n: number;
  children: React.ReactNode;
  className?: string;
  badge?: "right" | "left";
}) {
  return (
    <span className={cn("relative inline-flex rounded-md ring-2 ring-amber-500 ring-offset-2 ring-offset-white", className)}>
      {children}
      <span
        className={cn(
          "absolute -top-3 grid size-5 place-items-center rounded-full bg-amber-500 text-[11px] font-semibold text-white shadow",
          badge === "right" ? "-right-3" : "-left-3",
        )}
      >
        {n}
      </span>
    </span>
  );
}

/** A grey bar standing in for text the step does not need read. */
function Line({ w, className }: { w: string; className?: string }) {
  return <span className={cn("block h-2 rounded-full bg-neutral-200", className)} style={{ width: w }} />;
}

/** The browser around a screen, with the address the reader should see in theirs. */
function Shot({ url, label, children }: { url: string; label: string; children: React.ReactNode }) {
  return (
    <div
      role="img"
      aria-label={label}
      className="overflow-hidden rounded-xl border border-border bg-white text-[#242424] shadow-sm"
    >
      <div aria-hidden className="flex h-8 items-center gap-3 border-b border-neutral-200 bg-neutral-100 px-3">
        <span className="flex gap-1.5">
          <span className="size-2.5 rounded-full bg-neutral-300" />
          <span className="size-2.5 rounded-full bg-neutral-300" />
          <span className="size-2.5 rounded-full bg-neutral-300" />
        </span>
        <span className="min-w-0 flex-1 truncate rounded-md bg-white px-2 py-0.5 text-[11px] text-neutral-500">{url}</span>
      </div>
      <div aria-hidden className="relative select-none">
        {children}
      </div>
    </div>
  );
}

// ---- the Teams admin centre ----

function AdminChrome({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex min-h-[15rem] flex-col text-[12px]">
      <div className="flex h-9 items-center gap-3 bg-[#2b2b2b] px-3 text-white">
        <span className="grid grid-cols-3 gap-[2px]">
          {Array.from({ length: 9 }).map((_, i) => (
            <span key={i} className="size-[3px] rounded-full bg-white/80" />
          ))}
        </span>
        <span className="font-semibold">Microsoft Teams admin center</span>
        <span className="ml-auto grid size-6 place-items-center rounded-full border border-white/60 text-[9px]">AB</span>
      </div>
      <div className="flex flex-1">
        <div className="hidden w-9 shrink-0 flex-col items-center gap-3 bg-neutral-100 py-3 sm:flex">
          {Array.from({ length: 6 }).map((_, i) => (
            <span key={i} className={cn("size-3.5 rounded-sm", i === 4 ? "bg-[#5b5fc7]" : "bg-neutral-300")} />
          ))}
        </div>
        <div className="relative min-w-0 flex-1 px-5 py-4">{children}</div>
      </div>
    </div>
  );
}

/** Manage apps, with the Actions menu open on Upload new app. */
export function UploadMenuScreen() {
  return (
    <Shot
      url="admin.teams.microsoft.com/policies/manage-apps"
      label="The Teams admin centre's Manage apps page. The Actions menu at the top right is open, and its first item, Upload new app, is marked."
    >
      <AdminChrome>
        <div className="flex items-start justify-between gap-4">
          <p className="text-[20px] font-semibold leading-tight">Manage apps</p>
          <div className="relative flex flex-col items-end">
            <Hot n={1}>
              <span className="px-1 py-0.5 font-medium" style={{ color: PURPLE }}>
                Actions ⌄
              </span>
            </Hot>
            <div className="mt-3 w-52 rounded-md border border-neutral-200 bg-white py-1 shadow-lg">
              <div className="px-2 py-1">
                <Hot n={2} className="w-full" badge="left">
                  <span className="flex w-full items-center gap-2 px-1 py-0.5">
                    <span className="text-[14px] leading-none text-neutral-500">+</span> Upload new app
                  </span>
                </Hot>
              </div>
              <p className="flex items-center gap-2 px-3 py-1.5 text-neutral-600">
                <span className="text-[12px] leading-none">⚙</span> Org-wide app settings
              </p>
            </div>
          </div>
        </div>
        <div className="mt-3 space-y-2 pr-48">
          <Line w="100%" />
          <Line w="92%" />
          <Line w="70%" />
        </div>
        <div className="mt-5 w-40 space-y-2 rounded-md border border-neutral-200 p-3">
          <Line w="70%" className="bg-neutral-300" />
          <Line w="50%" />
        </div>
      </AdminChrome>
    </Shot>
  );
}

/** The Upload a custom app dialog that Upload new app opens. */
export function UploadDialogScreen() {
  return (
    <Shot
      url="admin.teams.microsoft.com/policies/manage-apps"
      label="The Upload a custom app dialog, with its Upload button marked. Upload opens a file picker, where you choose the file you downloaded."
    >
      <AdminChrome>
        <div className="absolute inset-0 bg-neutral-900/30" />
        <div className="relative mx-auto flex max-w-[19rem] flex-col items-center rounded-lg bg-white px-5 pb-5 pt-4 text-center shadow-xl">
          <span className="self-end text-[13px] leading-none text-neutral-500">✕</span>
          <svg viewBox="0 0 64 56" className="mt-1 h-12 w-14" aria-hidden>
            <rect x="14" y="4" width="36" height="46" rx="3" fill="#e8e6fb" transform="rotate(-10 32 27)" />
            <rect x="20" y="16" width="8" height="8" rx="1" fill="#f0a35e" transform="rotate(-10 32 27)" />
            <rect x="31" y="16" width="14" height="2.5" rx="1" fill="#a9a5ef" transform="rotate(-10 32 27)" />
            <rect x="31" y="22" width="12" height="2.5" rx="1" fill="#a9a5ef" transform="rotate(-10 32 27)" />
            <rect x="20" y="30" width="24" height="2.5" rx="1" fill="#a9a5ef" transform="rotate(-10 32 27)" />
          </svg>
          <p className="mt-2 text-[14px] font-semibold">Upload a custom app</p>
          <p className="mt-1.5 text-[11px] leading-snug text-neutral-600">
            Before you upload the app, make sure it has been tested completely.
          </p>
          <span className="mt-4">
            <Hot n={3}>
              <span className="flex items-center gap-1.5 rounded px-5 py-1.5 font-semibold text-white" style={{ background: PURPLE }}>
                <span className="leading-none">⤒</span> Upload
              </span>
            </Hot>
          </span>
        </div>
      </AdminChrome>
    </Shot>
  );
}

// ---- Teams ----

function TeamsChrome({
  rail,
  railHot,
  children,
}: {
  rail?: "apps" | "chat" | "teams";
  /** Ring the selected rail item with this number, when pressing it is one of the steps. */
  railHot?: number;
  children: React.ReactNode;
}) {
  const icons: { key: string; label: string }[] = [
    { key: "activity", label: "Activity" },
    { key: "chat", label: "Chat" },
    { key: "teams", label: "Teams" },
    { key: "calendar", label: "Calendar" },
    { key: "apps", label: "Apps" },
  ];
  return (
    <div className="flex min-h-[15rem] flex-col text-[12px]">
      <div className="flex h-9 items-center gap-3 border-b border-neutral-200 bg-[#ebebf5] px-3">
        <span className="grid grid-cols-3 gap-[2px]">
          {Array.from({ length: 9 }).map((_, i) => (
            <span key={i} className="size-[3px] rounded-full bg-neutral-500" />
          ))}
        </span>
        <span className="mx-auto hidden h-5 w-1/2 items-center rounded-md border border-neutral-300 bg-white px-2 text-[10px] text-neutral-400 sm:flex">
          Search
        </span>
        <span className="ml-auto grid size-6 place-items-center rounded-full bg-[#c7c8f0] text-[9px] font-semibold text-[#3d3e8f]">AB</span>
      </div>
      <div className="flex flex-1">
        <div className="flex w-14 shrink-0 flex-col items-center gap-2.5 bg-[#ebebf5] py-3">
          {icons.map((i) =>
            i.key === rail ? (
              <Selected key={i.key} label={i.label} hot={railHot} />
            ) : (
              <span key={i.key} className="flex flex-col items-center gap-0.5 text-[9px] text-neutral-500">
                <span className="size-4 rounded bg-neutral-300" />
                {i.label}
              </span>
            ),
          )}
        </div>
        <div className="relative min-w-0 flex-1">{children}</div>
      </div>
    </div>
  );
}

/** The rail item for the screen being shown, ringed when pressing it is a step. */
function Selected({ label, hot }: { label: string; hot?: number }) {
  const item = (
    <span className="flex flex-col items-center gap-0.5 px-0.5 text-[9px] font-semibold" style={{ color: PURPLE }}>
      <span className="size-4 rounded" style={{ background: PURPLE }} />
      {label}
    </span>
  );
  return hot ? (
    <Hot n={hot} badge="right">
      {item}
    </Hot>
  ) : (
    item
  );
}

/** Teams' Apps page, searched for attestTag, with the app's details open on Add. */
export function FindAppScreen() {
  return (
    <Shot
      url="teams.microsoft.com"
      label="Microsoft Teams with Apps chosen in the left rail, attestTag typed into the search box, and the app's details open with the Add button marked."
    >
      <TeamsChrome rail="apps" railHot={1}>
        <div className="flex h-full">
          <div className="w-44 shrink-0 space-y-3 border-r border-neutral-200 p-3">
            <p className="text-[14px] font-semibold">Apps</p>
            <Hot n={2} className="w-full">
              <span className="flex w-full items-center rounded-md border border-neutral-300 bg-white px-2 py-1 text-[11px]">
                attestTag<span className="ml-px h-3 w-px bg-neutral-800" />
              </span>
            </Hot>
            <div className="flex items-center gap-2 rounded-md bg-neutral-100 p-2">
              <AppIcon />
              <div className="min-w-0">
                <p className="truncate text-[11px] font-semibold">attestTag</p>
                <p className="truncate text-[10px] text-neutral-500">attest_tag</p>
              </div>
            </div>
          </div>
          <div className="min-w-0 flex-1 p-4">
            <div className="rounded-lg border border-neutral-200 p-4 shadow-sm">
              <div className="flex items-center gap-3">
                <AppIcon size="lg" />
                <div className="min-w-0">
                  <p className="text-[14px] font-semibold">attestTag</p>
                  <p className="text-[10px] text-neutral-500">attest_tag</p>
                </div>
              </div>
              <span className="mt-3 inline-block">
                <Hot n={3}>
                  <span className="rounded px-5 py-1 font-semibold text-white" style={{ background: PURPLE }}>
                    Add
                  </span>
                </Hot>
              </span>
              <p className="mt-3 text-[11px] text-neutral-600">An AI teammate that answers in your channels and chats.</p>
              <div className="mt-2 space-y-1.5">
                <Line w="90%" />
                <Line w="65%" />
              </div>
            </div>
          </div>
        </div>
      </TeamsChrome>
    </Shot>
  );
}

/** A team's Manage team → Apps tab, where Get more apps adds an app to that team. */
export function TeamAppsScreen() {
  return (
    <Shot
      url="teams.microsoft.com"
      label="A team's Manage team page on its Apps tab, with Get more apps marked. That is where an app is added to one team."
    >
      <TeamsChrome rail="teams">
        <div className="space-y-3 p-4">
          <div className="flex items-center gap-2">
            <span className="grid size-6 place-items-center rounded bg-[#f3d6d8] text-[10px] font-semibold text-[#a4262c]">AC</span>
            <p className="text-[14px] font-semibold">Acme</p>
            <span className="text-neutral-400">›</span>
            <p className="text-neutral-600">Manage team</p>
          </div>
          <div className="flex gap-4 border-b border-neutral-200 text-[11px]">
            <span className="pb-1.5 text-neutral-500">Members</span>
            <span className="pb-1.5 text-neutral-500">Channels</span>
            <span className="pb-1.5 text-neutral-500">Settings</span>
            <span className="border-b-2 pb-1.5 font-semibold" style={{ borderColor: PURPLE, color: PURPLE }}>
              Apps
            </span>
          </div>
          <Hot n={1}>
            <span className="flex items-center gap-1.5 rounded-md border border-neutral-300 px-3 py-1 font-medium">
              <span className="leading-none">+</span> Get more apps
            </span>
          </Hot>
          <div className="space-y-2 pt-1">
            {["OneNote", "Planner", "SharePoint"].map((name) => (
              <div key={name} className="flex items-center gap-2">
                <span className="size-5 rounded bg-neutral-200" />
                <span className="text-[11px] text-neutral-600">{name}</span>
              </div>
            ))}
          </div>
        </div>
      </TeamsChrome>
    </Shot>
  );
}

/** The chat with the bot: the code sent, and the answer that says it worked. */
export function LinkChatScreen() {
  return (
    <Shot
      url="teams.microsoft.com"
      label="A chat with attestTag. The message link ABCD-EFGH has been sent, and attestTag answers: Connected."
    >
      <TeamsChrome rail="chat">
        <div className="flex h-full flex-col">
          <div className="flex items-center gap-2 border-b border-neutral-200 px-4 py-2">
            <AppIcon />
            <p className="font-semibold">attestTag</p>
          </div>
          <div className="flex-1 space-y-3 px-4 py-3">
            <div className="flex justify-end">
              <Hot n={1}>
                <span className="rounded-lg bg-[#e8ebfa] px-3 py-1.5 font-mono text-[11px]">link ABCD-EFGH</span>
              </Hot>
            </div>
            <div className="flex items-start gap-2">
              <AppIcon />
              <div className="max-w-[85%] rounded-lg bg-neutral-100 px-3 py-1.5 leading-snug">
                <p className="text-[10px] font-semibold text-neutral-500">attestTag</p>
                <p>
                  Connected. This Microsoft Teams organisation now belongs to <b>Acme</b> on attest_tag.
                  Mention me in a channel, or message me here.
                </p>
              </div>
            </div>
          </div>
          <div className="m-3 flex items-center rounded-md border border-neutral-300 px-3 py-1.5 text-[11px] text-neutral-400">
            Type a message
            <span className="ml-auto text-[13px] leading-none" style={{ color: PURPLE }}>
              ➤
            </span>
          </div>
        </div>
      </TeamsChrome>
    </Shot>
  );
}

/** A channel post that mentions the bot, and its answer in the same reply chain. */
export function MentionScreen() {
  return (
    <Shot
      url="teams.microsoft.com"
      label="A post in a channel that begins with an @attestTag mention and asks how long refunds take, and attestTag's answer underneath it."
    >
      <TeamsChrome rail="teams">
        <div className="space-y-3 p-4">
          <p className="text-[13px] font-semibold">
            General <span className="font-normal text-neutral-400">· Acme</span>
          </p>
          <div className="rounded-lg border border-neutral-200">
            <div className="flex items-start gap-2 p-3">
              <span className="grid size-6 shrink-0 place-items-center rounded-full bg-[#c7c8f0] text-[9px] font-semibold text-[#3d3e8f]">
                AN
              </span>
              <div className="min-w-0">
                <p className="text-[10px] font-semibold text-neutral-500">Ana</p>
                <p className="flex flex-wrap items-center gap-x-2.5 pt-1">
                  <Hot n={1}>
                    <span className="rounded px-0.5 font-semibold" style={{ color: PURPLE }}>
                      attestTag
                    </span>
                  </Hot>
                  <span>how long do refunds take?</span>
                </p>
              </div>
            </div>
            <div className="flex items-start gap-2 border-t border-neutral-200 bg-neutral-50 p-3">
              <AppIcon />
              <div className="min-w-0">
                <p className="text-[10px] font-semibold text-neutral-500">attestTag</p>
                <p>Refunds take five working days from the day we receive the item.</p>
              </div>
            </div>
            <p className="border-t border-neutral-200 px-3 py-1.5 text-[11px]" style={{ color: PURPLE }}>
              Reply
            </p>
          </div>
        </div>
      </TeamsChrome>
    </Shot>
  );
}
