// Where the Teams steps happen, in one place for the connect dialog and the step-by-step guide.
// Microsoft's own pages: a link opens them, nothing here fetches them.

/** Teams admin centre → Teams apps → Manage apps, where Actions → Upload new app lives. */
export const TEAMS_MANAGE_APPS_URL = "https://admin.teams.microsoft.com/policies/manage-apps";

/** Teams itself. It opens the web client, which offers the desktop app if one is installed. */
export const TEAMS_URL = "https://teams.microsoft.com/";

/** The step-by-step guide with pictures. Public, so it can be sent to a Teams admin who has no
 *  account here. A console path: the Link it goes into adds the /admin base. */
export const TEAMS_GUIDE_PATH = "/help/teams/";

/** What the app is called in Teams, and so what people type after the @. */
export const TEAMS_APP_NAME = "attestTag";
