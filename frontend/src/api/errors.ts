// api/errors.ts — the typed API error both adapters throw (mock ↔ http parity).
//
// The FE face of the server's unified error envelope
// `{"error":{"code","message"}}` (docs/design/api-error-envelope.md). Lives in
// its own seam-neutral module so the mock adapter can throw the SAME class the
// real http client throws without importing the http layer.

/** The error every non-2xx API call rejects with. `status` is the HTTP
 * status; `code` (machine-readable snake_case) and `serverMessage` (human
 * text) come from the envelope body — HONEST-empty `""` when the body did not
 * carry the envelope (a proxy error page, a dropped body). `message` keeps
 * the historical `http <status> for <METHOD> <path>` format verbatim (pinned
 * by client.test.ts) so logs and legacy string-matching stay stable. */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly serverMessage: string;
  /** Seconds from a 429's `Retry-After` header; null when absent/unparseable.
   *
   * Carried as a FIELD rather than parsed out of `serverMessage` at the point
   * of use: the wait is a machine-readable header, and scraping it back out of
   * prose would break the moment that prose is reworded or localised. */
  readonly retryAfter: number | null;

  constructor(
    message: string,
    status: number,
    code: string,
    serverMessage: string,
    retryAfter: number | null = null,
  ) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.serverMessage = serverMessage;
    this.retryAfter = retryAfter;
  }
}

/** How long a 429 says to wait, in whole seconds. Falls back to 1 rather than 0
 * when the header is missing or junk: "retry in 0s" reads as "go ahead now",
 * which is the one thing a throttle must never say. */
export function retryAfterSeconds(e: unknown): number {
  const raw = e instanceof ApiError ? e.retryAfter : null;
  return raw !== null && Number.isFinite(raw) && raw > 0 ? Math.ceil(raw) : 1;
}

/** Parse a `Retry-After` header value (delta-seconds form). null when absent or
 * not a positive number — the HTTP-date form is not used by this server. */
export function parseRetryAfter(header: string | null): number | null {
  if (!header) return null;
  const secs = Number(header.trim());
  return Number.isFinite(secs) && secs > 0 ? secs : null;
}

/** The server's human-readable REASON for a rejection, or `""` when there is
 * none to show (a plain Error thrown outside the adapters, a body that did not
 * carry the envelope).
 *
 * 🔴 Callers must treat `""` as "fall back to your own i18n copy", NEVER as the
 * message: an empty error line is worse than a generic one. The reason this
 * exists at all is that the doc-cap refusals carry real INSTRUCTIONS (how far
 * over the limit you are, what the cap is, that what is already stored is not
 * truncated, delete stale content first) — every word of which used to die at
 * the seam because the cards stored a boolean. Do not narrow this back to a
 * flag; the flag is what the user could already see.
 *
 * `message` is deliberately NOT used as a second fallback: it is the
 * `http <status> for <METHOD> <path>` log format, which is not readable copy. */
export function serverMessageOf(e: unknown): string {
  return e instanceof ApiError ? e.serverMessage : "";
}

/** True when `e` is an API rejection with HTTP status `status` — the one way
 * callers branch on an error's status (deleteRole's 409 有成員在線上 case).
 * Falls back to the historical `http <status>` message regex for a plain
 * Error thrown outside the adapters (defense in depth, not a contract). */
export function isHttpStatus(e: unknown, status: number): boolean {
  if (e instanceof ApiError) return e.status === status;
  return e instanceof Error && new RegExp(`\\bhttp ${status}\\b`).test(e.message);
}
