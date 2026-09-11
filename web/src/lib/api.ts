/**
 * The one way this app talks to Athanor.
 *
 * Every call carries the session cookie and nothing else: `credentials:
 * "same-origin"` is the default for same-origin fetch, and the cookie is
 * HttpOnly, so no code here can read or send the key itself. In development
 * Vite proxies these paths to the Go server (vite.config.ts) rather than
 * enabling CORS, because a cross-origin session cookie would have to be
 * SameSite=None and that is the property stopping CSRF against every write.
 */

/** What the server said when it refused. Carries the status so a caller can
 *  tell "you may not" from "that does not exist" without matching on text. */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
  get isUnauthorized() {
    return this.status === 401;
  }
  get isForbidden() {
    return this.status === 403;
  }
  get isNotFound() {
    return this.status === 404;
  }
  /** A conflict is the workflow refusing, not the server failing: a stale
   *  hash, an unsigned plan, a follow that already runs. Worth a distinct
   *  presentation, so it gets a distinct question. */
  get isConflict() {
    return this.status === 409;
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: {
      ...(init?.body ? { "Content-Type": "application/json" } : {}),
      ...init?.headers,
    },
  });
  const text = await res.text();
  if (!res.ok) {
    let message = text || res.statusText;
    try {
      const parsed = JSON.parse(text) as { error?: string };
      if (parsed.error) message = parsed.error;
    } catch {
      // A non-JSON body is the message.
    }
    throw new ApiError(res.status, message);
  }
  if (!text) return undefined as T;
  return JSON.parse(text) as T;
}

export const api = {
  get: <T>(path: string) => request<T>(path),
  post: <T>(path: string, body?: unknown) =>
    request<T>(path, { method: "POST", body: body === undefined ? undefined : JSON.stringify(body) }),
  patch: <T>(path: string, body?: unknown) =>
    request<T>(path, { method: "PATCH", body: body === undefined ? undefined : JSON.stringify(body) }),
  del: <T>(path: string) => request<T>(path, { method: "DELETE" }),
};
