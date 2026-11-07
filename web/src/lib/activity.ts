/**
 * Who is acting: the person or the app. Requests the app makes by itself
 * (polling, background refreshes) carry X-Janus-Activity: background, and
 * the server does not count them as activity for the idle sign-out; a
 * dashboard left open must still time out.
 */
let lastInput = Date.now();
const ACTIVE_WINDOW_MS = 30_000;
let listening = false;

export function markActivity(): void {
  lastInput = Date.now();
}

/** Start listening for keyboard, pointer, scroll and touch input (idempotent). */
export function trackActivity(): void {
  if (listening || typeof window === 'undefined') return;
  listening = true;
  for (const ev of ['keydown', 'pointerdown', 'wheel', 'touchstart', 'scroll'] as const) {
    window.addEventListener(ev, markActivity, { passive: true, capture: true });
  }
}

/** True when the person used the page in the last 30 seconds. */
export function personActive(now = Date.now()): boolean {
  return now - lastInput < ACTIVE_WINDOW_MS;
}

export function lastActivityAt(): number {
  return lastInput;
}

let lastForeground = Date.now();

/** Called by the API client for every request that counts as activity. */
export function markForegroundRequest(): void {
  lastForeground = Date.now();
}

/** When the server last saw the person (its idle clock starts here). */
export function lastForegroundAt(): number {
  return lastForeground;
}
