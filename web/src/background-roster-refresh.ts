// Invalidate only the roster that owns the committed join. Reuse the message
// feed's observations; no second polling loop or optimistic member objects.
const refreshes = new EventTarget();

export function refreshBackgroundRoster(groupId: string) {
  refreshes.dispatchEvent(new Event(groupId));
}

export function onBackgroundRosterRefresh(groupId: string, listener: () => void) {
  refreshes.addEventListener(groupId, listener);
  return () => refreshes.removeEventListener(groupId, listener);
}
