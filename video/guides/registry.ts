import type { LongForm } from "../types";
import { DEEP_DIVE } from "./deep-dive";
import { ASK } from "./ask";
import { MEMORY } from "./memory";
import { PERSONAL } from "./personal";
import { ACCESS } from "./access";
import { AUTOMATION } from "./automation";
import { REACH } from "./reach";
import { GUARDRAILS } from "./guardrails";
import { DOCUMENTS } from "./documents";
import { COST } from "./cost";
import { SIGNIN } from "./signin";
import { API } from "./api";

/**
 * Every long-form walkthrough, by id. One list, so the renderer and the poster
 * tool resolve an id the same way and a guide added here is known to both.
 *
 * The deep dive is the one on the site. Everything after it is console-only
 * (see `Copy.consoleOnly` in the site's src/lib/walkthroughs.server.ts) and
 * records in the organisation the deep dive founded — README, "The other
 * walkthroughs".
 */
export const LONG_FORM: LongForm[] = [
  DEEP_DIVE,
  // In Slack
  ASK,
  MEMORY,
  PERSONAL,
  ACCESS,
  AUTOMATION,
  // The console
  REACH,
  GUARDRAILS,
  DOCUMENTS,
  COST,
  SIGNIN,
  API,
];
