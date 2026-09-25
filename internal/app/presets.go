package app

import "strings"

// Preset describes one service the console can connect with a Connect button. It carries
// everything the dialog and the proxy need: credential shape, default hosts, a test call,
// hints for the admin, and usage notes the model reads.
type Preset struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Category      string            `json:"category"`
	CredType      string            `json:"cred_type"` // bearer | header | basic | gcp_sa | oauth2_cc | oauth_user | mcp | custom
	Hosts         []string          `json:"hosts"`
	PathPrefixes  []string          `json:"path_prefixes,omitempty"`  // narrows a shared host: Calendar and Drive both live on www.googleapis.com
	AuthURL       string            `json:"auth_url,omitempty"`       // oauth_user: where the person consents
	TokenURL      string            `json:"token_url,omitempty"`      // oauth_user: where the code and the refresh are spent
	HeaderName    string            `json:"header_name,omitempty"`    // header type: which header carries the token
	HeaderPrefix  string            `json:"header_prefix,omitempty"`  // e.g. "Bearer " or "Token token="
	ExtraHeaders  []Header          `json:"extra_headers,omitempty"`  // additional secret headers (Datadog app key)
	StaticHeaders map[string]string `json:"static_headers,omitempty"` // non-secret headers (Notion-Version)
	Scopes        string            `json:"scopes,omitempty"`         // gcp_sa OAuth scopes
	SecretLabel   string            `json:"secret_label"`
	SecretHint    string            `json:"secret_hint"`
	Placeholder   string            `json:"placeholder"`
	DocsURL       string            `json:"docs_url"`
	Test          TestCall          `json:"test"`
	Notes         string            `json:"notes"`
	HasPack       bool              `json:"has_pack"`
	// Options are the parts of a service an admin may leave out. Google is one OAuth client and
	// one sign-in per person whether or not it covers mail, so mail and calendar cannot be
	// separate presets without asking everybody to consent three times. What differs between a
	// connection that only reads a calendar and one that also drafts mail is the scopes each
	// person is asked for and the paths the proxy will carry — so an option names both, the
	// console offers them as checkboxes, and the connection ends up allowlisted to exactly what
	// was ticked. A preset with no options is the whole service, as before.
	Options []PresetOption `json:"options,omitempty"`
	// Serves is where on a shared host this service lives, for a preset whose connections are
	// made without path prefixes. It allows nothing: what a connection reaches is still its own
	// hosts and prefixes. It says what the credential is *for*, which is the one thing the proxy
	// cannot read off a connection that claims all of www.googleapis.com — and the thing it needs
	// when somebody's own account was the first choice for a Drive URL and they have not
	// connected it, so that a shared Drive credential may answer and one kept for some other
	// Google API may not.
	Serves []string `json:"-"`
}

// presetServes says whether a connection's preset names path as its own service.
func presetServes(conn *Connection, path string) bool {
	pr := presetByID(conn.Preset)
	if pr == nil {
		return false
	}
	for _, p := range pr.Serves {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// PresetOption is one such part. ReadScopes are asked for whenever it is on; WriteScopes are
// added only where the admin allowed writing, which is how per-service write control survives a
// connection whose allowed methods are necessarily shared by every service on it.
type PresetOption struct {
	ID           string   `json:"id"`
	Label        string   `json:"label"`
	Hint         string   `json:"hint,omitempty"`
	Hosts        []string `json:"hosts,omitempty"`
	PathPrefixes []string `json:"path_prefixes,omitempty"`
	ReadScopes   string   `json:"read_scopes,omitempty"`
	WriteScopes  string   `json:"write_scopes,omitempty"`
	WriteLabel   string   `json:"write_label,omitempty"` // what writing means here: "book and move events"
	Default      bool     `json:"default,omitempty"`
	DefaultWrite bool     `json:"default_write,omitempty"`
	// Examples are things a person can say once this part is connected, in their words rather
	// than the API's. They are what `!connect` and the Connect card show: somebody who has just
	// been asked to hand over their calendar deserves to know what it buys them, and "Google
	// Workspace is connected" on its own has never told anybody what to type next.
	Examples []string `json:"examples,omitempty"`
}

// connectionOptions reports which of a preset's parts a connection actually carries, read back
// off what it is allowed to reach. The selection is not stored anywhere — the hosts and prefixes
// are the selection — so this is how anything downstream of the console knows that a particular
// Google connection is calendar-and-contacts rather than all three.
func connectionOptions(pr *Preset, conn *Connection) []PresetOption {
	if pr == nil || conn == nil || len(pr.Options) == 0 {
		return nil
	}
	hosts := map[string]bool{}
	for _, h := range conn.AllowedHosts {
		hosts[strings.ToLower(h)] = true
	}
	prefixes := map[string]bool{}
	for _, p := range conn.PathPrefixes {
		prefixes[p] = true
	}
	var out []PresetOption
	for _, o := range pr.Options {
		// Prefixes first, for the same reason resolveOptions writes them: two parts can share a
		// host, and then the host alone cannot tell them apart. A connection that narrows
		// nothing carries everything its hosts allow.
		on := false
		if len(o.PathPrefixes) > 0 && len(prefixes) > 0 {
			for _, p := range o.PathPrefixes {
				on = on || prefixes[p]
			}
		} else {
			for _, h := range o.Hosts {
				on = on || hosts[strings.ToLower(h)]
			}
		}
		if on {
			out = append(out, o)
		}
	}
	return out
}

// connectExamples is what to tell somebody they can now ask for, given what this connection
// actually covers. Empty for a preset with no parts, which is every preset that is one service.
func connectExamples(conn *Connection) []string {
	var out []string
	for _, o := range connectionOptions(presetByID(conn.Preset), conn) {
		out = append(out, o.Examples...)
	}
	return out
}

// ConnectionOption is an admin's answer to one of them: on, and whether writing is allowed.
type ConnectionOption struct {
	ID    string `json:"id"`
	Write bool   `json:"write"`
}

// connectionOptionState is the admin's answer read back off a connection that already exists:
// which parts it carries, and whether each of them may write. Which parts are on is readable
// from the hosts and prefixes alone; whether writing was allowed lives only in the scopes, which
// is why this needs them handed in.
//
// The console's edit form is the caller that matters. Without this it fell back to the preset's
// own defaults, so re-opening a connection to change one part silently rewrote another — a
// Google connection that could draft mail came back with the box clear, and saving dropped the
// grant everybody had already consented to.
func connectionOptionState(pr *Preset, conn *Connection, scopes string) []ConnectionOption {
	have := map[string]bool{}
	for _, s := range strings.Fields(scopes) {
		have[s] = true
	}
	out := []ConnectionOption{}
	for _, o := range connectionOptions(pr, conn) {
		// Every scope the write box asks for has to be present. A part with no write scopes at
		// all is read-only by construction and never reports writing.
		write := o.WriteScopes != ""
		for _, s := range strings.Fields(o.WriteScopes) {
			write = write && have[s]
		}
		out = append(out, ConnectionOption{ID: o.ID, Write: write})
	}
	return out
}

// baseScopes are asked for whatever the admin ticked: they identify the account being connected,
// which is how a person's second sign-in is recognised as the same person.
const baseScopes = "openid email"

// resolveOptions turns the ticked boxes into the three things that actually enforce them: the
// hosts and path prefixes the proxy will carry, and the scopes each person is asked to consent
// to. An option nobody ticked contributes nothing, so its host is not reachable and its scope is
// never requested — Google refuses the call even if the model composes one.
//
// Methods are deliberately not narrowed for a read-only option. They belong to the whole
// connection, and a read-only calendar still has to POST /calendar/v3/freeBusy to answer "when is
// everyone free" (see readShapedPOSTs). Scopes are the thing with the right granularity here:
// without calendar.events Google itself refuses the booking.
func resolveOptions(pr *Preset, sel []ConnectionOption) (hosts, prefixes []string, scopes string) {
	if pr == nil || len(pr.Options) == 0 {
		return nil, nil, ""
	}
	byID := map[string]ConnectionOption{}
	for _, s := range sel {
		byID[strings.ToLower(strings.TrimSpace(s.ID))] = s
	}
	scopeList := []string{baseScopes}
	// Walked in preset order rather than in the order they arrived, so two admins who tick the
	// same boxes get byte-identical connections.
	for _, o := range pr.Options {
		s, on := byID[o.ID]
		if !on {
			continue
		}
		hosts = appendNew(hosts, o.Hosts...)
		prefixes = appendNew(prefixes, o.PathPrefixes...)
		if o.ReadScopes != "" {
			scopeList = append(scopeList, o.ReadScopes)
		}
		if s.Write && o.WriteScopes != "" {
			scopeList = append(scopeList, o.WriteScopes)
		}
	}
	if len(hosts) == 0 {
		return nil, nil, ""
	}
	return hosts, prefixes, strings.Join(scopeList, " ")
}

func appendNew(dst []string, add ...string) []string {
	for _, a := range add {
		found := false
		for _, d := range dst {
			if d == a {
				found = true
			}
		}
		if !found {
			dst = append(dst, a)
		}
	}
	return dst
}

type TestCall struct {
	Method string `json:"method"`
	Path   string `json:"path"` // relative to the first host; {project_id} is filled from a GCP key
	Body   string `json:"body,omitempty"`
}

// The catalogue itself, in the order the console draws it: grouped by category,
// and within a category the one most teams reach for first. The console groups
// on `Category` in the order it meets them here, so this slice is what a Connect
// list looks like — move an entry and the dialog moves with it.
var Presets = []Preset{
	{ID: "clickup", Name: "ClickUp", Category: "Issue tracking", CredType: "header", Hosts: []string{"api.clickup.com"},
		HeaderName: "Authorization", HeaderPrefix: "",
		SecretLabel: "Personal API token", SecretHint: "ClickUp Settings → Apps → API Token. Create it from a dedicated account so its activity is auditable.", Placeholder: "pk_…",
		DocsURL: "https://developer.clickup.com/docs/authentication", Test: TestCall{Method: "GET", Path: "/api/v2/user"},
		Notes: "ClickUp REST API v2 at https://api.clickup.com/api/v2. A task needs a list id: get it from clickup_lists in one call, never by walking /team, /space and /folder. Useful: GET /team (workspaces), GET /team/{team_id}/task?statuses[]=…&assignees[]=… (search), GET /task/{id}, POST /list/{list_id}/task (create), PUT /task/{id} (update). Dates are unix ms.", HasPack: true},
	{ID: "linear", Name: "Linear", Category: "Issue tracking", CredType: "header", Hosts: []string{"api.linear.app"}, HeaderName: "Authorization", HeaderPrefix: "",
		SecretLabel: "API key", SecretHint: "Linear → Settings → API → Personal API keys, from a dedicated seat.", Placeholder: "lin_api_…",
		DocsURL: "https://developers.linear.app/docs/graphql/working-with-the-graphql-api", Test: TestCall{Method: "POST", Path: "/graphql", Body: `{"query":"{ viewer { id name } }"}`},
		Notes: "Linear GraphQL at POST https://api.linear.app/graphql with {query, variables}. Example: { issues(filter:{state:{name:{eq:\"In Progress\"}}}, first:20) { nodes { identifier title assignee { name } url } } }."},
	{ID: "atlassian", Name: "Jira and Confluence", Category: "Issue tracking", CredType: "basic", Hosts: []string{"your-site.atlassian.net"},
		SecretLabel: "Email and API token", SecretHint: "Create an API token at id.atlassian.com for a dedicated Atlassian user; enter email as user and the token as password. Replace the host with your site.", Placeholder: "",
		DocsURL: "https://developer.atlassian.com/cloud/jira/platform/basic-auth-for-rest-apis/", Test: TestCall{Method: "GET", Path: "/rest/api/3/myself"},
		Notes: "Jira: GET /rest/api/3/search?jql=project=ABC AND status!=Done ORDER BY updated DESC&maxResults=20, GET /rest/api/3/issue/{key}. Confluence: GET /wiki/rest/api/content/search?cql=text~\"refund\"&limit=10."},
	{ID: "asana", Name: "Asana", Category: "Issue tracking", CredType: "bearer", Hosts: []string{"app.asana.com"},
		SecretLabel: "Personal access token", SecretHint: "Asana → Settings → Apps → Developer apps → personal access token. Create it from a dedicated account so its activity is auditable.", Placeholder: "2/1234…",
		DocsURL: "https://developers.asana.com/docs/personal-access-token", Test: TestCall{Method: "GET", Path: "/api/1.0/users/me"},
		Notes: "Asana at /api/1.0; every id is a gid. GET /api/1.0/workspaces, GET /api/1.0/projects?workspace={gid}, GET /api/1.0/tasks?project={gid}&opt_fields=name,completed,due_on,assignee.name,permalink_url, GET /api/1.0/tasks/{gid}?opt_fields=notes,custom_fields, GET /api/1.0/workspaces/{gid}/tasks/search?text=… (search needs a paid plan). Ask for opt_fields or you get bare gids."},
	{ID: "github", Name: "GitHub", Category: "Code", CredType: "bearer", Hosts: []string{"api.github.com"},
		SecretLabel: "Personal access token", SecretHint: "Fine-grained PAT from a dedicated GitHub user with read access to the repos you want, plus issues/PR write if you want the bot to comment.", Placeholder: "github_pat_…",
		DocsURL: "https://docs.github.com/en/rest/authentication", Test: TestCall{Method: "GET", Path: "/user"},
		StaticHeaders: map[string]string{"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28"},
		Notes:         "GitHub REST API. GET /search/issues?q=repo:org/name+is:pr+is:open, GET /repos/{o}/{r}/pulls/{n}, GET /repos/{o}/{r}/commits, POST /repos/{o}/{r}/issues/{n}/comments (write).", HasPack: true},
	{ID: "gitlab", Name: "GitLab", Category: "Code", CredType: "header", Hosts: []string{"gitlab.com"}, HeaderName: "PRIVATE-TOKEN",
		SecretLabel: "Access token", SecretHint: "GitLab → Settings → Access tokens: a personal, group or project token with read_api, from a dedicated account. Self-managed: replace the host with your instance.", Placeholder: "glpat-…",
		DocsURL: "https://docs.gitlab.com/api/rest/authentication/", Test: TestCall{Method: "GET", Path: "/api/v4/user"},
		Notes: "GitLab REST v4 at /api/v4. A project id is the numeric id or the URL-encoded path, e.g. group%2Fsub%2Fproject. GET /api/v4/projects?membership=true&search=…, GET /api/v4/projects/{id}/merge_requests?state=opened&order_by=updated_at, GET /api/v4/projects/{id}/issues?state=opened, GET /api/v4/projects/{id}/pipelines?ref=main, POST /api/v4/projects/{id}/merge_requests/{iid}/notes (write). Paging is ?per_page=&page=."},
	{ID: "bitbucket", Name: "Bitbucket", Category: "Code", CredType: "basic", Hosts: []string{"api.bitbucket.org"},
		SecretLabel: "Email and API token", SecretHint: "Create an API token at id.atlassian.com for a dedicated Atlassian user with repository read; enter that email as user and the token as password.", Placeholder: "",
		DocsURL: "https://support.atlassian.com/bitbucket-cloud/docs/using-api-tokens/", Test: TestCall{Method: "GET", Path: "/2.0/user"},
		Notes: "Bitbucket Cloud v2: GET /2.0/repositories/{workspace}?role=member, GET /2.0/repositories/{workspace}/{repo}/pullrequests?state=OPEN, GET /2.0/repositories/{workspace}/{repo}/pullrequests/{id}/diffstat, GET /2.0/repositories/{workspace}/{repo}/commits/{branch}. Filters go in ?q=, e.g. q=state=\"MERGED\" AND updated_on>=2026-01-01."},
	{ID: "gcp_logs", Name: "GCP logs (read-only)", Category: "Monitoring", CredType: "gcp_sa", Hosts: []string{"logging.googleapis.com", "monitoring.googleapis.com"},
		Scopes:      "https://www.googleapis.com/auth/logging.read https://www.googleapis.com/auth/monitoring.read",
		SecretLabel: "Service-account key (JSON)", SecretHint: "Create a service account with roles/logging.viewer and roles/monitoring.viewer, then paste its JSON key. The proxy exchanges it for short-lived access tokens; the key never leaves this server.", Placeholder: `{"type": "service_account", …}`,
		DocsURL: "https://cloud.google.com/logging/docs/reference/v2/rest/v2/entries/list",
		Test:    TestCall{Method: "POST", Path: "/v2/entries:list", Body: `{"resourceNames":["projects/{project_id}"],"pageSize":1}`},
		Notes:   "Cloud Logging: POST https://logging.googleapis.com/v2/entries:list with {resourceNames:[\"projects/ID\"], filter, orderBy:\"timestamp desc\", pageSize}. Filters use the Logging query language, e.g. resource.type=\"k8s_container\" AND severity>=ERROR AND timestamp>=\"2026-09-01T00:00:00Z\". Cloud Monitoring: GET https://monitoring.googleapis.com/v3/projects/ID/timeSeries?filter=…&interval.startTime=…&interval.endTime=….", HasPack: true},
	{ID: "sentry", Name: "Sentry", Category: "Monitoring", CredType: "bearer", Hosts: []string{"sentry.io"},
		SecretLabel: "Auth token", SecretHint: "Sentry → Settings → Developer Settings → Internal Integrations → token with project:read and event:read.", Placeholder: "sntrys_…",
		DocsURL: "https://docs.sentry.io/api/auth/", Test: TestCall{Method: "GET", Path: "/api/0/organizations/"},
		Notes: "Sentry API at https://sentry.io/api/0. GET /organizations/{org}/issues/?query=is:unresolved&statsPeriod=24h, GET /issues/{id}/, GET /issues/{id}/events/latest/, GET /organizations/{org}/projects/.", HasPack: true},
	{ID: "datadog", Name: "Datadog", Category: "Monitoring", CredType: "header", Hosts: []string{"api.datadoghq.com"}, HeaderName: "DD-API-KEY",
		ExtraHeaders: []Header{{Name: "DD-APPLICATION-KEY"}},
		SecretLabel:  "API key", SecretHint: "Organization Settings → API Keys, plus an Application Key with read scopes (second field). Change the host for EU (api.datadoghq.eu).", Placeholder: "",
		DocsURL: "https://docs.datadoghq.com/api/latest/authentication/", Test: TestCall{Method: "GET", Path: "/api/v1/validate"},
		Notes: "Datadog: POST /api/v2/logs/events/search {filter:{query,from,to},page:{limit}}, GET /api/v1/monitor?monitor_tags=…, GET /api/v1/query?from=…&to=…&query=avg:system.cpu.user{*}."},
	{ID: "grafana", Name: "Grafana", Category: "Monitoring", CredType: "bearer", Hosts: []string{"your-org.grafana.net"},
		SecretLabel: "Service account token", SecretHint: "Grafana → Administration → Service accounts → add a token with the Viewer role. Replace the host with your Grafana Cloud stack, or your own Grafana.", Placeholder: "glsa_…",
		DocsURL: "https://grafana.com/docs/grafana/latest/developers/http_api/", Test: TestCall{Method: "GET", Path: "/api/org"},
		Notes: "Grafana HTTP API: GET /api/search?type=dash-db&query=checkout (dashboards), GET /api/dashboards/uid/{uid}, GET /api/datasources, GET /api/alertmanager/grafana/api/v2/alerts (what is firing now), POST /api/ds/query {from,to,queries:[{datasource:{uid},expr,…}]} to run one query against a datasource."},
	{ID: "newrelic", Name: "New Relic", Category: "Monitoring", CredType: "header", Hosts: []string{"api.newrelic.com"}, HeaderName: "Api-Key",
		SecretLabel: "User API key", SecretHint: "New Relic → Administration → API keys → create a User key. EU accounts: change the host to api.eu.newrelic.com.", Placeholder: "NRAK-…",
		DocsURL: "https://docs.newrelic.com/docs/apis/intro-apis/new-relic-api-keys/", Test: TestCall{Method: "POST", Path: "/graphql", Body: `{"query":"{ actor { user { name } } }"}`},
		Notes: "NerdGraph is one endpoint: POST /graphql with {query}. NRQL goes through it: { actor { account(id: 123) { nrql(query: \"SELECT count(*) FROM TransactionError SINCE 1 hour ago FACET appName\") { results } } } }. Entities: { actor { entitySearch(query: \"domain = 'APM'\") { results { entities { name guid } } } } }."},
	{ID: "pagerduty", Name: "PagerDuty", Category: "Monitoring", CredType: "header", Hosts: []string{"api.pagerduty.com"}, HeaderName: "Authorization", HeaderPrefix: "Token token=",
		StaticHeaders: map[string]string{"Accept": "application/vnd.pagerduty+json;version=2"},
		SecretLabel:   "API token", SecretHint: "Integrations → API Access Keys (read-only key).", Placeholder: "",
		DocsURL: "https://developer.pagerduty.com/docs/authentication", Test: TestCall{Method: "GET", Path: "/abilities"},
		Notes: "PagerDuty: GET /incidents?statuses[]=triggered&statuses[]=acknowledged&since=…, GET /oncalls?schedule_ids[]=…, GET /services."},
	{ID: "gcp", Name: "Google Cloud", Category: "Cloud", CredType: "gcp_sa",
		Hosts:       []string{"cloudresourcemanager.googleapis.com", "compute.googleapis.com", "run.googleapis.com", "storage.googleapis.com", "bigquery.googleapis.com"},
		Scopes:      "https://www.googleapis.com/auth/cloud-platform",
		SecretLabel: "Service-account key (JSON)", SecretHint: "A service account with the roles it should have on the projects it should see — roles/viewer to look, more only if you want it to change things; paste its JSON key. The proxy exchanges it for short-lived access tokens and the key never leaves this server. Add a host per API you want reachable.", Placeholder: `{"type": "service_account", …}`,
		DocsURL: "https://cloud.google.com/iam/docs/keys-create-delete", Test: TestCall{Method: "GET", Path: "/v1/projects/{project_id}"},
		Notes: "Google Cloud REST, one host per API and always with the project id in the path. Resource Manager: GET https://cloudresourcemanager.googleapis.com/v1/projects. Compute: GET https://compute.googleapis.com/compute/v1/projects/{p}/aggregated/instances. Cloud Run: GET https://run.googleapis.com/v2/projects/{p}/locations/{loc}/services. Storage: GET https://storage.googleapis.com/storage/v1/b?project={p}. BigQuery: POST https://bigquery.googleapis.com/bigquery/v2/projects/{p}/queries {\"query\":\"…\",\"useLegacySql\":false,\"maxResults\":50} — what this key may do is whatever its service account's IAM roles allow, so a call beyond them is refused by IAM rather than by the proxy."},
	{ID: "aws", Name: "AWS", Category: "Cloud", CredType: "aws_sigv4",
		Hosts:       []string{"sts.amazonaws.com", "monitoring.us-east-1.amazonaws.com", "logs.us-east-1.amazonaws.com"},
		SecretLabel: "Access key", SecretHint: "An IAM user with ReadOnlyAccess, or a policy narrower than that: access key id, secret access key, and the region you work in. The proxy signs every request (SigV4) — the secret key is never sent. Add one host per service and region you want reachable, e.g. ec2.eu-west-1.amazonaws.com.", Placeholder: "AKIA…",
		DocsURL: "https://docs.aws.amazon.com/IAM/latest/UserGuide/id_credentials_access-keys.html",
		Test:    TestCall{Method: "GET", Path: "/?Action=GetCallerIdentity&Version=2011-06-15"},
		Notes:   "AWS APIs come in two shapes, and the host decides the service and the region. Query protocol (STS, EC2, CloudWatch, ELB, RDS), answering in XML: GET https://sts.amazonaws.com/?Action=GetCallerIdentity&Version=2011-06-15, GET https://monitoring.eu-west-1.amazonaws.com/?Action=DescribeAlarms&Version=2010-08-01&StateValue=ALARM, GET https://ec2.eu-west-1.amazonaws.com/?Action=DescribeInstances&Version=2016-11-15. JSON protocol (CloudWatch Logs, DynamoDB, Cost Explorer): POST / on the service host with Content-Type: application/x-amz-json-1.1 and X-Amz-Target naming the operation, e.g. X-Amz-Target: Logs_20140328.FilterLogEvents and body {\"logGroupName\":\"/aws/lambda/checkout\",\"filterPattern\":\"ERROR\",\"startTime\":1725000000000,\"limit\":20}. Times are unix ms. sts.amazonaws.com and iam.amazonaws.com are global; everything else needs the regional host."},
	{ID: "azure", Name: "Azure", Category: "Cloud", CredType: "oauth2_cc", Hosts: []string{"management.azure.com"},
		Scopes:      "https://management.azure.com/.default",
		SecretLabel: "App registration", SecretHint: "Entra ID → App registrations: the application (client) id, a client secret, and https://login.microsoftonline.com/{tenant-id}/oauth2/v2.0/token as the token URL. Give the app the Reader role on the subscriptions it should see.", Placeholder: "",
		DocsURL: "https://learn.microsoft.com/entra/identity-platform/v2-oauth2-client-creds-grant", Test: TestCall{Method: "GET", Path: "/subscriptions?api-version=2022-12-01"},
		Notes: "Azure Resource Manager, and every call needs an api-version. GET /subscriptions?api-version=2022-12-01, GET /subscriptions/{sub}/resourceGroups?api-version=2021-04-01, GET /subscriptions/{sub}/resources?$filter=resourceType eq 'Microsoft.Web/sites'&api-version=2021-04-01, GET /subscriptions/{sub}/providers/Microsoft.Insights/eventtypes/management/values?api-version=2015-04-01&$filter=eventTimestamp ge '2026-09-01T00:00:00Z' (activity log). Metrics: GET /{resource-id}/providers/Microsoft.Insights/metrics?api-version=2018-01-01&metricnames=…&timespan=…."},
	{ID: "circleci", Name: "CircleCI", Category: "Builds and infrastructure", CredType: "header", Hosts: []string{"circleci.com"}, HeaderName: "Circle-Token",
		SecretLabel: "Personal API token", SecretHint: "CircleCI → User Settings → Personal API Tokens, from a dedicated account with read access to the projects.", Placeholder: "",
		DocsURL: "https://circleci.com/docs/api-developers-guide/", Test: TestCall{Method: "GET", Path: "/api/v2/me"},
		Notes: "CircleCI API v2; a project slug is vcs/org/repo, e.g. gh/acme/api. GET /api/v2/project/{slug}/pipeline?branch=main, GET /api/v2/pipeline/{id}/workflow, GET /api/v2/workflow/{id}/job (status=failed is the one you want), GET /api/v2/project/{slug}/{job_number}/artifacts."},
	{ID: "vercel", Name: "Vercel", Category: "Builds and infrastructure", CredType: "bearer", Hosts: []string{"api.vercel.com"},
		SecretLabel: "Access token", SecretHint: "Vercel → Account Settings → Tokens; scope it to the team whose projects the bot may read.", Placeholder: "",
		DocsURL: "https://vercel.com/docs/rest-api", Test: TestCall{Method: "GET", Path: "/v2/user"},
		Notes: "Vercel REST: GET /v9/projects, GET /v6/deployments?projectId=…&limit=10&state=ERROR, GET /v13/deployments/{idOrUrl}, GET /v3/deployments/{id}/events (build log lines). Add &teamId=team_… on every call when the token is personal and the project belongs to a team. Timestamps are unix ms."},
	{ID: "cloudflare", Name: "Cloudflare", Category: "Builds and infrastructure", CredType: "bearer", Hosts: []string{"api.cloudflare.com"},
		SecretLabel: "API token", SecretHint: "Cloudflare → My Profile → API Tokens → Create Token with read permissions (Zone:Read, Analytics:Read). Not the Global API Key, which can do everything.", Placeholder: "",
		DocsURL: "https://developers.cloudflare.com/fundamentals/api/get-started/create-token/", Test: TestCall{Method: "GET", Path: "/client/v4/user/tokens/verify"},
		Notes: "Cloudflare v4: GET /client/v4/zones?name=acme.com (zone ids), GET /client/v4/zones/{zone_id}/dns_records?type=A, GET /client/v4/accounts/{account_id}/workers/scripts. Traffic, cache and firewall numbers live in the GraphQL analytics API instead: POST /client/v4/graphql with {query, variables} over the httpRequests1hGroups datasets."},
	{ID: "zendesk", Name: "Zendesk", Category: "Support", CredType: "basic", Hosts: []string{"your-org.zendesk.com"},
		SecretLabel: "Email and API token", SecretHint: "Admin Center → Apps and integrations → Zendesk API → add an API token. The user is the agent email with /token appended (sam@acme.com/token) and the password is the token. Replace the host with your subdomain.", Placeholder: "",
		DocsURL: "https://developer.zendesk.com/api-reference/introduction/security-and-auth/", Test: TestCall{Method: "GET", Path: "/api/v2/users/me.json"},
		Notes: "Zendesk Support: GET /api/v2/search.json?query=type:ticket status:open tags:billing (the search syntax agents use), GET /api/v2/tickets/{id}.json, GET /api/v2/tickets/{id}/comments.json, GET /api/v2/views/{id}/tickets.json, GET /api/v2/users/{id}.json."},
	{ID: "intercom", Name: "Intercom", Category: "Support", CredType: "bearer", Hosts: []string{"api.intercom.io"},
		StaticHeaders: map[string]string{"Intercom-Version": "2.11", "Accept": "application/json"},
		SecretLabel:   "Access token", SecretHint: "Intercom → Settings → Integrations → Developer Hub → your app → Authentication, then copy the workspace access token.", Placeholder: "dG9r…",
		DocsURL: "https://developers.intercom.com/docs/build-an-integration/learn-more/authentication/", Test: TestCall{Method: "GET", Path: "/me"},
		Notes: "Intercom REST: POST /conversations/search {query:{field:\"open\",operator:\"=\",value:true},pagination:{per_page:20}}, GET /conversations/{id}, POST /contacts/search {query:{field:\"email\",operator:\"=\",value:\"…\"}}, GET /articles. Timestamps are unix seconds."},
	{ID: "notion", Name: "Notion", Category: "Knowledge and docs", CredType: "bearer", Hosts: []string{"api.notion.com"},
		StaticHeaders: map[string]string{"Notion-Version": "2022-06-28"},
		SecretLabel:   "Integration token", SecretHint: "notion.so/my-integrations → internal integration; share the pages it may read with it.", Placeholder: "ntn_…",
		DocsURL: "https://developers.notion.com/docs/authorization", Test: TestCall{Method: "GET", Path: "/v1/users/me"},
		Notes: "Notion API: POST /v1/search {query, page_size}, GET /v1/pages/{id}, GET /v1/blocks/{id}/children, POST /v1/databases/{id}/query."},
	{ID: "gdrive", Name: "Google Drive", Category: "Knowledge and docs", CredType: "gcp_sa", Hosts: []string{"www.googleapis.com"},
		Scopes:      "https://www.googleapis.com/auth/drive",
		Serves:      []string{"/drive/v3", "/upload/drive/v3"},
		SecretLabel: "Service-account key (JSON)", SecretHint: "Share the folders with the service account's email — Viewer to read, Editor if you want it to write. Paste its JSON key.", Placeholder: `{"type": "service_account", …}`,
		DocsURL: "https://developers.google.com/drive/api/guides/about-auth", Test: TestCall{Method: "GET", Path: "/drive/v3/about?fields=user"},
		Notes: "The company's Drive: the folders shared with this service account, readable by anyone in the channel without connecting an account of their own. " +
			"Find files with drive_search and read one with drive_read, which search every Drive in the channel and say whose each file is. " +
			"By hand, for a search they cannot express (a folder's contents by date, say): GET /drive/v3/files?q=…&fields=files(id,name,mimeType,modifiedTime,webViewLink)" +
			"&supportsAllDrives=true&includeItemsFromAllDrives=true with trashed=false in q. Writing needs Editor on the file or folder: PATCH /drive/v3/files/{id} " +
			"(rename, move), POST /upload/drive/v3/files?uploadType=media (create) — a share the account only reads is refused by Drive rather than by the proxy."},
	{ID: "airtable", Name: "Airtable", Category: "Knowledge and docs", CredType: "bearer", Hosts: []string{"api.airtable.com"},
		SecretLabel: "Personal access token", SecretHint: "airtable.com/create/tokens → scopes data.records:read and schema.bases:read, granted only to the bases the bot may read.", Placeholder: "pat…",
		DocsURL: "https://airtable.com/developers/web/guides/personal-access-tokens", Test: TestCall{Method: "GET", Path: "/v0/meta/whoami"},
		Notes: "Airtable: GET /v0/meta/bases (base ids), GET /v0/meta/bases/{baseId}/tables (fields and views), GET /v0/{baseId}/{tableIdOrName}?maxRecords=20&view=Grid%20view&filterByFormula={Status}='Open'. Field names in a formula are case-sensitive and go in {braces}."},
	{ID: "figma", Name: "Figma", Category: "Knowledge and docs", CredType: "header", Hosts: []string{"api.figma.com"}, HeaderName: "X-Figma-Token",
		SecretLabel: "Personal access token", SecretHint: "Figma → Settings → Security → personal access tokens, with read scopes for files and comments.", Placeholder: "figd_…",
		DocsURL: "https://www.figma.com/developers/api#access-tokens", Test: TestCall{Method: "GET", Path: "/v1/me"},
		Notes: "Figma: the file key is the segment after /design/ or /file/ in a Figma URL. GET /v1/files/{key}?depth=1 (pages, without every node), GET /v1/files/{key}/comments, GET /v1/files/{key}/versions, GET /v1/teams/{team_id}/projects, GET /v1/projects/{project_id}/files, GET /v1/images/{key}?ids={node-id}&format=png."},
	{ID: "hubspot", Name: "HubSpot", Category: "Go-to-market", CredType: "bearer", Hosts: []string{"api.hubapi.com"},
		SecretLabel: "Private app token", SecretHint: "HubSpot → Settings → Integrations → Private Apps → create one with read scopes for contacts, companies, deals.", Placeholder: "pat-na1-…",
		DocsURL: "https://developers.hubspot.com/docs/api/private-apps", Test: TestCall{Method: "GET", Path: "/crm/v3/objects/contacts?limit=1"},
		// The search rules are spelled out because runs kept learning them the expensive way, a
		// round at a time: conditions split across filter groups (ORed — 37k contacts instead of
		// 15), no sort (oldest first — a run paged forward from 2025 and never reached this week),
		// notes that would not attach for want of an association type id.
		Notes: "HubSpot CRM v3. Search: POST /crm/v3/objects/{contacts|companies|deals}/search with {filterGroups:[{filters:[{propertyName, operator, value}]}], properties, sorts:[{propertyName:\"createdate\", direction:\"DESCENDING\"}], limit (max 200), after}. " +
			"Filters in one group are ANDed and separate groups are ORed, so every condition of one query goes in a single group; without sorts results come oldest first; dates filter as ISO 8601 (2026-01-31T00:00:00Z). " +
			"Read one: GET /crm/v3/objects/{type}/{id}?properties=…&associations=companies. Many at once: POST /crm/v3/objects/{type}/batch/read or /batch/update with {inputs:[{id, properties}]}, up to 100. " +
			"Create a note or task already linked by passing associations:[{to:{id}, types:[{associationCategory:\"HUBSPOT_DEFINED\", associationTypeId:N}]}] (note→contact 202, note→company 190, task→contact 204, task→company 192); a task's due date is hs_timestamp.",
		HasPack: true},
	{ID: "stripe", Name: "Stripe", Category: "Billing", CredType: "bearer", Hosts: []string{"api.stripe.com"},
		SecretLabel: "Restricted key (read-only)", SecretHint: "Developers → API keys → Create restricted key with read permissions only.", Placeholder: "rk_live_…",
		DocsURL: "https://docs.stripe.com/keys", Test: TestCall{Method: "GET", Path: "/v1/balance"},
		Notes: "Stripe: GET /v1/customers/search?query=email:'x@y.com', GET /v1/invoices?customer=cus_…&limit=10, GET /v1/charges/{id}, GET /v1/subscriptions?customer=…."},
	{ID: "google", Name: "Google Workspace", Category: "Personal accounts", CredType: "oauth_user",
		Hosts: []string{"www.googleapis.com", "gmail.googleapis.com", "people.googleapis.com"},
		PathPrefixes: []string{"/calendar/v3", "/gmail/v1", "/drive/v3", "/upload/drive/v3",
			"/v1/people", "/v1/otherContacts", "/v1/contactGroups"},
		AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token",
		// Everything, for a connection made before the options existed or by an API client that
		// sent none. TestPresetScopesMatchOptions keeps this equal to every option ticked with
		// writing on, so the two cannot drift apart.
		Scopes: "openid email https://www.googleapis.com/auth/gmail.readonly https://www.googleapis.com/auth/gmail.compose " +
			"https://www.googleapis.com/auth/calendar.readonly https://www.googleapis.com/auth/calendar.events " +
			"https://www.googleapis.com/auth/drive.readonly https://www.googleapis.com/auth/drive " +
			"https://www.googleapis.com/auth/contacts.readonly https://www.googleapis.com/auth/contacts.other.readonly " +
			"https://www.googleapis.com/auth/directory.readonly",
		Options: []PresetOption{
			{ID: "gmail", Label: "Gmail", Default: true,
				Hint:         "Search and read the asker's own mail.",
				Hosts:        []string{"gmail.googleapis.com"},
				PathPrefixes: []string{"/gmail/v1"},
				ReadScopes:   "https://www.googleapis.com/auth/gmail.readonly",
				WriteScopes:  "https://www.googleapis.com/auth/gmail.compose",
				// gmail.compose is drafts *and* sending — Google has no draft-only scope — so
				// the label says so. The bot only ever drafts, but the grant is wider than the
				// bot's habits, and the consent screen the person sees will say "send mail as
				// you" whatever this box claims.
				WriteLabel: "write drafts and send mail as you",
				Examples: []string{
					"did anyone at Acme reply about the invoice this week?",
					"draft a reply to the last mail from finance",
				}},
			{ID: "calendar", Label: "Calendar", Default: true, DefaultWrite: true,
				Hint:         "Read calendars and find a time everyone is free.",
				Hosts:        []string{"www.googleapis.com"},
				PathPrefixes: []string{"/calendar/v3"},
				ReadScopes:   "https://www.googleapis.com/auth/calendar.readonly",
				WriteScopes:  "https://www.googleapis.com/auth/calendar.events",
				WriteLabel:   "book, move and cancel events",
				Examples: []string{
					"what's on my calendar tomorrow?",
					"set up a 30 minute call at 7am with everyone on this thread",
					"find the next morning priya@acme.com and I are both free and book it",
				}},
			// Off by default, and the only part here that is. Gmail reaches mail and Calendar
			// reaches events, but a Drive grant reaches every file the person can open — which is
			// usually the widest thing anyone is asked for in this dialog. An admin who wants it
			// can say so; nobody should get it because they accepted a default.
			{ID: "drive", Label: "Drive",
				Hint:         "Search and read the asker's own files.",
				Hosts:        []string{"www.googleapis.com"},
				PathPrefixes: []string{"/drive/v3", "/upload/drive/v3"},
				ReadScopes:   "https://www.googleapis.com/auth/drive.readonly",
				// Google has no "edit what they already have" scope: drive.file reaches only files
				// this app itself created, which is no use for a document somebody wrote last week.
				// So the write box is the whole of Drive, and the label says so rather than letting
				// the consent screen be the first place anybody finds out.
				WriteScopes: "https://www.googleapis.com/auth/drive",
				WriteLabel:  "create, edit and delete any of your files",
				Examples: []string{
					"find the Q3 planning doc in my Drive",
					"what does my onboarding checklist say about laptops?",
					"add a row to the bottom of my expenses sheet",
				}},
			{ID: "contacts", Label: "Contacts and directory", Default: true,
				Hint:         "Turn a name into an address, so \"Priya at Acme\" is enough to reach the right person.",
				Hosts:        []string{"people.googleapis.com"},
				PathPrefixes: []string{"/v1/people", "/v1/otherContacts", "/v1/contactGroups"},
				ReadScopes: "https://www.googleapis.com/auth/contacts.readonly https://www.googleapis.com/auth/contacts.other.readonly " +
					"https://www.googleapis.com/auth/directory.readonly",
				Examples: []string{
					"what's Priya at Acme's email address?",
					"invite Priya at Acme to that call",
				}},
		},
		SecretLabel: "OAuth client id and secret",
		SecretHint: "Google Cloud → APIs and Services → Credentials → OAuth client ID, type Web application, with the redirect URI shown below. " +
			"Enable the Gmail API, the Google Calendar API, the Google Drive API and the People API on the same project — whichever of them you ticked above. " +
			"Set the consent screen to Internal if this is a Workspace " +
			"domain: an External app using the Gmail scopes needs Google's verification and an annual third-party security assessment first. " +
			"Everyone signs in for themselves, so this pair reaches nobody's mail on its own.",
		Placeholder: "1234-abc.apps.googleusercontent.com",
		DocsURL:     "https://developers.google.com/identity/protocols/oauth2/web-server",
		Notes: "Gmail and Calendar, always as the person who asked — 'me' and 'primary' are them, and there is no way to read anyone else's " +
			"mailbox through this. Calendar v3 at https://www.googleapis.com/calendar/v3, times RFC3339 with an offset: " +
			"GET /calendar/v3/users/me/calendarList, GET /calendar/v3/calendars/primary/events?timeMin=…&timeMax=…&singleEvents=true&orderBy=startTime&maxResults=50, " +
			"GET /calendar/v3/calendars/primary/events?q=…. An ask that already names an hour — 'set up a call at 7am with everyone here' — is not a " +
			"scheduling problem: book it at that hour and leave the calendars alone. Go looking only when the ask is for a time nobody has fixed yet " +
			"('find a slot', 'when are we all free'). Attendees come from the ask the same way. An address written out in the ask is the attendee " +
			"already: use it as it stands, look nobody up, and note that nothing behind it has to be a Slack account — a customer or a candidate is " +
			"invited exactly like a colleague. A mention is the case that needs resolving, and the people on a thread are the ones who have spoken in " +
			"it, whose ids are in the messages you were given: pass the lot to get_user in one call for their addresses. A bare name — 'Priya at " +
			"Acme' — is the People API's job: GET https://people.googleapis.com/v1/people:searchContacts?query=priya&readMask=names,emailAddresses,organizations, " +
			"GET https://people.googleapis.com/v1/otherContacts:search?query=orle&readMask=names,emailAddresses for people only ever mailed, and " +
			"GET https://people.googleapis.com/v1/people:searchDirectoryPeople?query=orle&readMask=names,emailAddresses,organizations&sources=DIRECTORY_SOURCE_TYPE_DOMAIN_PROFILE " +
			"for colleagues. Both search endpoints are cache-backed and answer the very first query of a session with nothing: send the same URL once " +
			"with query= empty to warm it, then search. Search on the distinctive word rather than the whole phrase — 'priya', not 'priya at acme' " +
			"— and read the company off organizations to tell two people of the same name apart. Names are a guess, so say which address you picked and " +
			"never invent one: if the search comes back empty, ask rather than guessing at firstname@company.com. Say in your reply who you " +
			"invited, so anyone missing can be added. To find a slot ask the calendars themselves rather than reading events one by one: " +
			"POST /calendar/v3/freeBusy {\"timeMin\":…,\"timeMax\":…,\"timeZone\":…,\"items\":[{\"id\":\"someone@acme.com\"},{\"id\":\"primary\"}]} returns the busy " +
			"blocks for each, and a colleague whose calendar is not shared comes back with an error in that entry rather than as free — report that, " +
			"never read it as free. freeBusy only looks, so it runs without asking. For a fixed hour on whichever day suits everyone, ask freeBusy once " +
			"across the whole span (timeMin now, timeMax a week out) and pick the day out of the busy blocks yourself; one call per day spends the turn's " +
			"tool rounds for nothing. An hour named without a zone is that hour where the asker is: take their tz from get_user and write the offset into " +
			"dateTime rather than sending UTC. Book with " +
			"POST /calendar/v3/calendars/primary/events?sendUpdates=all&conferenceDataVersion=1 {\"summary\",\"description\",\"start\":{\"dateTime\",\"timeZone\"},\"end\":{…},\"attendees\":[{\"email\"}]," +
			"\"conferenceData\":{\"createRequest\":{\"requestId\":\"<any unique string>\",\"conferenceSolutionKey\":{\"type\":\"hangoutsMeet\"}}}} — conferenceDataVersion=1 is what " +
			"makes the Meet link real: without it Google drops conferenceData and hands back a perfectly successful event with no video link. Read the " +
			"link out of hangoutLink in the response and never compose a meet.google.com URL yourself. " +
			"Move with PATCH /calendar/v3/calendars/primary/events/{id}. Gmail v1 at https://gmail.googleapis.com/gmail/v1: " +
			"GET /gmail/v1/users/me/messages?q=from:sam newer_than:7d is:unread&maxResults=20 (the q is Gmail's own search syntax), then " +
			"GET /gmail/v1/users/me/messages/{id}?format=metadata&metadataHeaders=From&metadataHeaders=Subject&metadataHeaders=Date for a header line " +
			"or format=full for the body, whose parts[].body.data is base64url. Draft with POST /gmail/v1/users/me/drafts {\"message\":{\"raw\":\"<base64url RFC822>\"}}. " +
			"Drive v3 at https://www.googleapis.com/drive/v3, and always the asker's own files — there is no way to reach anyone else's. " +
			"Find and read files with drive_search and drive_read rather than by hand: they look in the company's Drive as well and say whose each " +
			"file is. By hand, search with GET /drive/v3/files?q=…&fields=files(id,name,mimeType,modifiedTime,webViewLink)&orderBy=modifiedTime desc: q is Drive's own " +
			"query language, where name contains 'onboarding' matches titles and fullText contains 'refund' matches contents, and trashed=false belongs " +
			"on every search or deleted files come back. Add supportsAllDrives=true&includeItemsFromAllDrives=true to see shared drives, which is where " +
			"most company files are; without them a search quietly returns only My Drive. Read a Google Doc with " +
			"GET /drive/v3/files/{id}/export?mimeType=text/plain (a Sheet with text/csv, which is its first sheet and nothing else), and an uploaded file " +
			"with GET /drive/v3/files/{id}?alt=media. Create with POST /upload/drive/v3/files?uploadType=media, replace contents with " +
			"PATCH /upload/drive/v3/files/{id}?uploadType=media, rename or move with PATCH /drive/v3/files/{id}. A Google Doc has no bytes to write: " +
			"changing one means exporting it, editing the text and sending it back, so say that is what you are doing rather than claiming an in-place edit. " +
			"Two files can share a name and an edit to the wrong one does not look wrong, so name the file and its webViewLink before changing anything, " +
			"and never delete a file the ask did not name."},
	{ID: "custom", Name: "Custom HTTP API", Category: "Custom", CredType: "custom", Hosts: []string{},
		SecretLabel: "Secret", SecretHint: "Any service with an HTTP API. Pick the credential type that matches how it authenticates.", DocsURL: "",
		Test: TestCall{Method: "GET", Path: "/"}, Notes: ""},
	{ID: "mcp", Name: "Custom MCP server", Category: "Custom", CredType: "mcp", Hosts: []string{},
		SecretLabel: "Bearer token", SecretHint: "A remote MCP server over streamable HTTP. Its tools are loaded on demand, when a request involves it, and offered to the model with a prefix.", DocsURL: "https://modelcontextprotocol.io",
		Test: TestCall{Method: "POST", Path: "/"}, Notes: ""},
}

func presetByID(id string) *Preset {
	for i := range Presets {
		if Presets[i].ID == id {
			return &Presets[i]
		}
	}
	return nil
}
