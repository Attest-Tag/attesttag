import type {
  Connection,
  ConnectionInput,
  ConnectionOption,
  CredType,
  Header,
  Preset,
  PresetOption,
  Secret,
} from "@/lib/api";

// Everything the Connect dialog edits, in one flat object so both tabs and the
// test bar read the same state. `secret` holds every credential shape; only
// the fields for the chosen `credType` are sent.
export type SecretForm = {
  token: string;
  user: string;
  password: string;
  headerName: string;
  headerPrefix: string;
  clientId: string;
  clientSecret: string;
  tokenUrl: string;
  scopes: string;
  saJson: string;
  mcpUrl: string;
  awsKeyId: string;
  awsSecret: string;
  awsRegion: string;
  awsService: string;
  awsSessionToken: string;
};

/** How much of what a connection can do waits for a person in Slack. */
export type WritesMode = "confirm" | "auto" | "all";

export type ConnectionForm = {
  bundleId: number;
  name: string;
  credType: CredType;
  hosts: string[];
  pathPrefixes: string[];
  methods: string[];
  writes: WritesMode;
  notes: string;
  /** May an approved access request spend this credential. Off unless an admin turns it on. */
  allow_grants: boolean;
  secret: SecretForm;
  /** Values for a preset's `extra_headers`, keyed by header name. */
  headerValues: Record<string, string>;
  /**
   * Which parts of a multi-service preset are on, keyed by option id. Empty for a preset with
   * no options, in which case nothing is sent and the server connects the whole service.
   */
  options: Record<string, { on: boolean; write: boolean }>;
  includePack: boolean;
  /**
   * OAuth client details for an MCP server without dynamic registration.
   * Sent with the sign-in call only, never on create or update.
   */
  oauth: OAuthClientForm;
};

export type OAuthClientForm = { clientId: string; clientSecret: string; scopes: string };

export const EMPTY_OAUTH: OAuthClientForm = { clientId: "", clientSecret: "", scopes: "" };

export const CRED_TYPES: { value: CredType; label: string; hint: string }[] = [
  { value: "bearer", label: "Bearer token", hint: "Authorization: Bearer <token>" },
  { value: "basic", label: "Basic auth", hint: "A username and password, base64 in the Authorization header." },
  { value: "header", label: "Custom header", hint: "The token goes in a header you name, with an optional prefix." },
  { value: "query", label: "Query parameter", hint: "The token rides on the query string under a name you give." },
  { value: "oauth2_cc", label: "OAuth2 client credentials", hint: "The proxy exchanges a client id and secret for short-lived access tokens." },
  { value: "gcp_sa", label: "Google service account", hint: "A JSON key; the proxy mints access tokens for the scopes below." },
  { value: "aws_sigv4", label: "AWS access key", hint: "The proxy signs each request with SigV4; the secret key itself never goes over the wire." },
  { value: "oauth_user", label: "Each person's own account", hint: "You register the OAuth client; everyone signs in for themselves, and the bot acts only as whoever asked." },
  { value: "mcp", label: "MCP server", hint: "A remote MCP server over streamable HTTP; its tools are offered to the model." },
];

export const EMPTY_SECRET: SecretForm = {
  token: "",
  user: "",
  password: "",
  headerName: "",
  headerPrefix: "",
  clientId: "",
  clientSecret: "",
  tokenUrl: "",
  scopes: "",
  saJson: "",
  mcpUrl: "",
  awsKeyId: "",
  awsSecret: "",
  awsRegion: "",
  awsService: "",
  awsSessionToken: "",
};

function presetCredType(preset: Preset): CredType {
  return preset.cred_type === "custom" ? "bearer" : preset.cred_type;
}

/** The options a preset suggests, before the admin touches them. */
export function defaultOptions(opts: PresetOption[] | null | undefined): ConnectionForm["options"] {
  return Object.fromEntries(
    (opts ?? []).map((o) => [o.id, { on: !!o.default, write: !!o.default_write }]),
  );
}

/**
 * Which options an existing connection has on, worked out from what it is allowed to reach: the
 * selection itself is not stored, and the hosts are. Writing is not readable this way — it lives
 * in the scopes each person already consented to — so it comes back as the preset's default and
 * an admin who changes it is re-asking everyone anyway.
 */
export function optionsFromConnection(
  c: Connection,
  opts: PresetOption[] | null | undefined,
): ConnectionForm["options"] {
  const hosts = new Set((c.allowed_hosts ?? []).map((h) => h.toLowerCase()));
  const prefixes = new Set(c.path_prefixes ?? []);
  // Prefixes first: two options can share a host, and then the host says nothing about which of
  // them is on. Hosts are the fallback for an option that narrows nothing.
  const on = (o: PresetOption) =>
    (o.path_prefixes ?? []).length > 0
      ? (o.path_prefixes ?? []).some((p) => prefixes.has(p))
      : (o.hosts ?? []).some((h) => hosts.has(h.toLowerCase()));
  return Object.fromEntries(
    (opts ?? []).map((o) => [o.id, { on: on(o), write: !!o.default_write }]),
  );
}

/** A fresh form for connecting `preset` into `bundleId`. */
export function formForPreset(preset: Preset, bundleId: number, packOn: boolean): ConnectionForm {
  const credType = presetCredType(preset);
  return {
    bundleId,
    name: preset.id === "custom" || preset.id === "mcp" ? "" : preset.name,
    credType,
    hosts: [...(preset.hosts ?? [])],
    // A preset that shares a host with another service carries the prefixes that tell them
    // apart. Showing them empty and saving them anyway makes the Advanced tab a liar.
    pathPrefixes: [...(preset.path_prefixes ?? [])],
    methods: [],
    writes: "confirm",
    notes: "",
    allow_grants: false,
    secret: {
      ...EMPTY_SECRET,
      headerName: preset.header_name ?? "",
      headerPrefix: preset.header_prefix ?? "",
      scopes: preset.scopes ?? "",
    },
    headerValues: Object.fromEntries((preset.extra_headers ?? []).map((h) => [h.name, ""])),
    options: defaultOptions(preset.options),
    includePack: preset.has_pack && packOn,
    oauth: { ...EMPTY_OAUTH },
  };
}

/** The form for editing an existing connection; secrets start empty. */
export function formForConnection(c: Connection, preset: Preset | undefined, packOn: boolean): ConnectionForm {
  const header = c.headers?.[0];
  return {
    bundleId: c.bundle_id,
    name: c.name,
    credType: (c.cred_type as CredType) || "bearer",
    hosts: [...(c.allowed_hosts ?? [])],
    pathPrefixes: [...(c.path_prefixes ?? [])],
    methods: [...(c.methods ?? [])],
    writes: c.writes === "auto" || c.writes === "all" ? c.writes : "confirm",
    notes: c.notes,
    allow_grants: !!c.allow_grants,
    secret: {
      ...EMPTY_SECRET,
      headerName: header?.name ?? preset?.header_name ?? "",
      headerPrefix: header?.prefix ?? preset?.header_prefix ?? "",
      scopes: preset?.scopes ?? "",
    },
    headerValues: Object.fromEntries((preset?.extra_headers ?? []).map((h) => [h.name, ""])),
    options: optionsFromConnection(c, preset?.options),
    includePack: !!preset?.has_pack && packOn,
    oauth: { ...EMPTY_OAUTH },
  };
}

/** The `secret` object for the wire, or undefined when nothing was typed. */
export function secretForWire(form: ConnectionForm): Secret | undefined {
  const s = form.secret;
  let out: Secret;
  switch (form.credType) {
    case "bearer":
      out = { token: s.token };
      break;
    case "basic":
      out = { user: s.user, password: s.password };
      break;
    case "header":
      out = { token: s.token, header_name: s.headerName };
      break;
    case "query":
      out = { token: s.token, header_name: s.headerName };
      break;
    case "oauth2_cc":
      out = {
        client_id: s.clientId,
        client_secret: s.clientSecret,
        token_url: s.tokenUrl,
        scopes: s.scopes,
      };
      break;
    case "gcp_sa":
      out = { sa_json: s.saJson, scopes: s.scopes };
      break;
    case "aws_sigv4":
      out = {
        aws_key_id: s.awsKeyId,
        aws_secret: s.awsSecret,
        aws_region: s.awsRegion,
        aws_service: s.awsService,
        aws_session_token: s.awsSessionToken,
      };
      break;
    case "oauth_user":
      // Flat on the wire; the server folds it into the connection's OAuth client and fills
      // in where to send people from the preset.
      out = { client_id: s.clientId, client_secret: s.clientSecret, scopes: s.scopes };
      break;
    case "mcp":
      out = { mcp_url: s.mcpUrl, token: s.token };
      break;
  }
  // Which fields actually make this a rotation. The rest — a header name, a token url, a scope
  // list — are settings that arrive prefilled when editing, and treating one of those as "the
  // user typed a new credential" sends a secret with the credential itself left blank, wiping
  // the stored one. Anything not listed here falls back to "every field", which is only right
  // for credential types whose fields are all secret.
  const rotationFields: Partial<Record<string, (keyof Secret)[]>> = {
    header: ["token"],
    query: ["token"],
    gcp_sa: ["sa_json"],
    aws_sigv4: ["aws_key_id", "aws_secret"],
    mcp: ["mcp_url", "token"],
    oauth2_cc: ["client_id", "client_secret", "token_url"],
    oauth_user: ["client_id", "client_secret"],
  };
  const meaningful: (keyof Secret)[] =
    rotationFields[form.credType] ?? (Object.keys(out) as (keyof Secret)[]);
  const typed = meaningful.some((k) => (out[k] ?? "").trim() !== "");
  return typed ? out : undefined;
}

/** Non-empty extra header values, or undefined. */
export function headerValuesForWire(form: ConnectionForm): Record<string, string> | undefined {
  const entries = Object.entries(form.headerValues).filter(([, v]) => v.trim() !== "");
  return entries.length > 0 ? Object.fromEntries(entries) : undefined;
}

/** The body for POST /api/bundles/{id}/connections, PUT /api/connections/{id} and the pre-save test. */
export function toInput(form: ConnectionForm, presetId: string): ConnectionInput {
  const headers: Header[] =
    form.credType === "header" && form.secret.headerName
      ? [{ name: form.secret.headerName, prefix: form.secret.headerPrefix }]
      : [];
  let hosts = form.hosts;
  // An MCP server's host is the URL's host; fill it in so the proxy's allow
  // list matches what the model will call.
  if (form.credType === "mcp" && form.secret.mcpUrl) {
    try {
      const h = new URL(form.secret.mcpUrl).host.toLowerCase();
      if (h && !hosts.includes(h)) hosts = [...hosts, h];
    } catch {
      // not a URL yet; the server will say so
    }
  }
  const input: ConnectionInput = {
    name: form.name.trim(),
    preset: presetId,
    cred_type: form.credType,
    allowed_hosts: hosts,
    path_prefixes: form.pathPrefixes,
    methods: form.methods,
    headers,
    writes: form.writes,
    notes: form.notes,
    allow_grants: form.allow_grants,
  };
  const secret = secretForWire(form);
  if (secret) input.secret = secret;
  const hv = headerValuesForWire(form);
  if (hv) input.header_values = hv;
  const options = optionsForWire(form);
  if (options) input.options = options;
  return input;
}

/**
 * The hosts and prefixes the ticked options come to, mirroring resolveOptions on the Go side.
 * The server recomputes this and does not trust what we send, but the Advanced tab shows these
 * two lists as what the connection may reach — so leaving them at the preset's full set while
 * only Calendar is ticked would have the dialog state something untrue.
 */
export function reachForOptions(
  preset: Preset,
  options: ConnectionForm["options"],
): { hosts: string[]; pathPrefixes: string[] } {
  const hosts: string[] = [];
  const pathPrefixes: string[] = [];
  for (const o of preset.options ?? []) {
    if (!options[o.id]?.on) continue;
    for (const h of o.hosts ?? []) if (!hosts.includes(h)) hosts.push(h);
    for (const p of o.path_prefixes ?? []) if (!pathPrefixes.includes(p)) pathPrefixes.push(p);
  }
  return { hosts, pathPrefixes };
}

/**
 * The ticked options, or undefined for a preset that has none — the server reads an absent list
 * as "the whole service", which is what every preset but Google means.
 */
export function optionsForWire(form: ConnectionForm): ConnectionOption[] | undefined {
  const on = Object.entries(form.options).filter(([, v]) => v.on);
  if (Object.keys(form.options).length === 0) return undefined;
  return on.map(([id, v]) => ({ id, write: v.write }));
}

/** Whether the form carries enough of a credential to create or test with. */
export function secretComplete(form: ConnectionForm): boolean {
  const s = form.secret;
  switch (form.credType) {
    case "bearer":
      return s.token.trim() !== "";
    case "basic":
      return s.user.trim() !== "" && s.password !== "";
    case "header":
      return s.token.trim() !== "";
    case "query":
      return s.token.trim() !== "" && s.headerName.trim() !== "";
    case "oauth2_cc":
      return s.clientId.trim() !== "" && s.clientSecret !== "" && s.tokenUrl.trim() !== "";
    case "gcp_sa":
      return s.saJson.trim() !== "";
    case "aws_sigv4":
      return s.awsKeyId.trim() !== "" && s.awsSecret !== "";
    case "oauth_user":
      return s.clientId.trim() !== "" && s.clientSecret !== "";
    case "mcp":
      return s.mcpUrl.trim() !== "";
  }
}

/** How a credential field that already holds a stored value is labelled. */
export type SavedState = "saved" | "replacing" | null;

/** What a field shows instead of the value nobody can read back. */
export const SAVED_PLACEHOLDER = "•••••••••• saved";

/**
 * Which credential inputs an existing connection already holds a value for. Nothing sealed can be
 * read back, so the dialog opens these blank — and blank reads as "there is nothing here" when the
 * truth is "it is not shown", which is how an admin comes to retype a credential they never needed
 * to. The server refuses to store a credential missing any of these (validateSecret), so a
 * connection that exists has every one of them. Optional fields — an MCP bearer token, an AWS
 * session token, a region, an extra header — are deliberately left out: calling one "saved" when it
 * may well hold nothing is a worse answer than saying nothing about it.
 */
export function savedSecretFields(credType: CredType): (keyof SecretForm)[] {
  switch (credType) {
    case "bearer":
    case "header":
      return ["token"];
    case "query":
      return ["token", "headerName"];
    case "basic":
      return ["user", "password"];
    case "oauth2_cc":
      return ["clientId", "clientSecret", "tokenUrl"];
    case "gcp_sa":
      return ["saJson"];
    case "aws_sigv4":
      return ["awsKeyId", "awsSecret"];
    case "oauth_user":
      return ["clientId", "clientSecret"];
    case "mcp":
      return ["mcpUrl"];
  }
}

/**
 * A stored field is shown as saved only while it is still empty. The moment something is typed the
 * mark says "replacing" instead, because that keystroke is a rotation: saving now overwrites the
 * credential rather than keeping it.
 */
export function savedState(
  saved: readonly (keyof SecretForm)[] | undefined,
  value: SecretForm,
  field: keyof SecretForm,
): SavedState {
  if (!saved?.includes(field)) return null;
  return value[field].trim() === "" ? "saved" : "replacing";
}

/** The same answer for a group of fields shown under one label: any keystroke makes it a rotation. */
export function savedGroupState(
  saved: readonly (keyof SecretForm)[],
  value: SecretForm,
): SavedState {
  if (saved.length === 0) return null;
  return saved.some((k) => value[k].trim() !== "") ? "replacing" : "saved";
}

/**
 * A path prefix as the proxy will read it: a path, with its leading slash. Typing "api/v2" and
 * getting a chip that reads "/api/v2" is the whole correction — the slash is added while the
 * admin is still looking at it, rather than a prefix being saved that can never match a URL path
 * and quietly allows nothing. The server refuses one without it either way.
 */
export function normalizePathPrefix(raw: string): string {
  const p = raw.trim();
  if (p === "") return "";
  return p.startsWith("/") ? p : `/${p}`;
}

/** Lower-case, strip scheme and trailing slash — the same cleanup the server does. */
export function normalizeHost(raw: string): string {
  let h = raw.trim().toLowerCase();
  h = h.replace(/^https?:\/\//, "").replace(/\/.*$/, "");
  return h;
}

/**
 * The path "Test connection" calls — mirrors testPath on the Go side. A preset's check call is
 * chosen for the whole service, so a connection narrowed to its own paths is checked against the
 * first of those instead, unless the preset's path already lives under it.
 */
export function testPath(preset: Preset | undefined, pathPrefixes: string[]): string {
  let path = preset?.test.path ?? "/";
  const first = pathPrefixes[0];
  if (first && !path.startsWith(first)) path = first;
  return path.startsWith("/") ? path : `/${path}`;
}

/** What the proxy will send, secret masked — mirrors curlExample on the Go side. */
export function maskedCurl(form: ConnectionForm, preset: Preset | undefined): string {
  const host = form.hosts[0] || "api.example.com";
  const path = testPath(preset, form.pathPrefixes).split("{")[0] || "/";
  const method = preset?.test.method || "GET";
  let auth = "";
  switch (form.credType) {
    case "bearer":
    case "mcp":
    case "gcp_sa":
    case "oauth2_cc":
      auth = `-H "Authorization: Bearer ••••••••"`;
      break;
    case "oauth_user":
      auth = `-H "Authorization: Bearer ••••••••"  # the asker's own token, not one shared token`;
      break;
    case "basic":
      auth = `-u "${form.secret.user || "user"}:••••••••"`;
      break;
    case "header":
      auth = `-H "${form.secret.headerName || "Authorization"}: ${form.secret.headerPrefix}••••••••"`;
      break;
    case "query":
      auth = `# plus ?${form.secret.headerName || "<param>"}=•••••••• on the query string`;
      break;
    case "aws_sigv4":
      auth = `-H "Authorization: AWS4-HMAC-SHA256 Credential=${form.secret.awsKeyId || "AKIA…"}/…, Signature=••••••••"`;
      break;
  }
  const extras = Object.keys(form.headerValues)
    .map((name) => `-H "${name}: ••••••••"`)
    .join(" ");
  const statics = Object.entries(preset?.static_headers ?? {})
    .map(([k, v]) => `-H "${k}: ${v}"`)
    .join(" ");
  const flags = [auth, extras, statics].filter(Boolean).join(" ");
  return `curl -X ${method} ${flags} \\\n  https://${host}${path}`;
}
