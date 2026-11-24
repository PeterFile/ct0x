import { decisionKey, validDecisionReference } from "./decision-api";
import type { DecisionAttempt, DecisionHistory, DecisionJournalEntry } from "./decision-api";

// Best-effort reload recovery on this origin in this tab. This is an index
// for the authoritative GET, not a write requirement or a receipt cache.
const storageKey = "f2-decision-answers-v1";
export const journalLimit = 32;
const maxLength = 64 * 1024;
const isDigest = (value: unknown) => typeof value === "string" && /^[0-9a-f]{64}$/.test(value);
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

function isEntry(value: unknown): value is DecisionJournalEntry {
  if (typeof value !== "object" || value === null || Array.isArray(value)) return false;
  const entry = value as Record<string, unknown>;
  if (!validDecisionReference(entry.ref)) return false;
  const size = Object.keys(entry).length;
  if (size === 2) return isDigest(entry.receiptDigest);
  return size === 4 && (entry.scope === "me" || entry.scope === "group")
    && typeof entry.key === "string" && uuid.test(entry.key) && isDigest(entry.digest);
}

export function loadDecisionJournal(): DecisionJournalEntry[] | null {
  try {
    const raw = sessionStorage.getItem(storageKey);
    if (raw === null) return [];
    if (raw.length > maxLength) return null;
    const entries: unknown = JSON.parse(raw);
    if (!Array.isArray(entries) || entries.length > journalLimit || !entries.every(isEntry)
      || new Set(entries.map(entry => decisionKey(entry.ref))).size !== entries.length) return null;
    return entries;
  } catch { return null; }
}

function persist(entries: DecisionJournalEntry[]) {
  try {
    const raw = JSON.stringify(entries);
    if (raw.length > maxLength) return false;
    sessionStorage.setItem(storageKey, raw);
    return true;
  } catch { return false; }
}

const sameAttempt = (entry: DecisionJournalEntry, attempt: DecisionAttempt) => "key" in entry
  && entry.key === attempt.key && entry.scope === attempt.scope && entry.digest === attempt.digest
  && decisionKey(entry.ref) === decisionKey(attempt.ref);

export function journalDecisionAttempt(attempt: DecisionAttempt) {
  const entries = loadDecisionJournal();
  const key = decisionKey(attempt.ref);
  if (!entries || entries.some(entry => decisionKey(entry.ref) === key)) return null;
  let evicted: DecisionJournalEntry | undefined;
  if (entries.length === journalLimit) {
    const oldest = entries.findIndex(entry => "receiptDigest" in entry);
    if (oldest === -1) return null; // Keep unresolved records; the new attempt can still live in memory.
    evicted = entries.splice(oldest, 1)[0];
  }
  return persist([...entries, attempt]) ? { evicted } : null;
}

export function settleDecisionAttempt(attempt: DecisionAttempt, history?: DecisionHistory) {
  const entries = loadDecisionJournal();
  const index = entries?.findIndex(entry => sameAttempt(entry, attempt)) ?? -1;
  if (!entries || index < 0) return false;
  entries.splice(index, 1, ...(history ? [history] : []));
  return persist(entries);
}
