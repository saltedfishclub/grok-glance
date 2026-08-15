/**
 * REST client for the glance API.
 *
 * Every call is same-origin and relies on the `__Host-` session cookie, so
 * nothing here handles tokens — except the bootstrap token, which is a
 * query-string credential by design (it has to work before any cookie exists).
 */

export interface Status {
  enrolled: boolean;
  authenticated: boolean;
}

export interface Enrollment {
  /** Base32 TOTP secret, shown so an authenticator can be set up by hand. */
  secret: string;
  /** `otpauth://` URI to render as a QR code. */
  uri: string;
}

export interface SessionMeta {
  sessionId?: string;
  cwd?: string;
  title?: string;
  model?: string;
  hostname?: string;
  version?: string;
}

export interface AgentSummary {
  id: string;
  keyName: string;
  session: SessionMeta;
  label: string;
  connectedAt: string;
  lastActivity: string;
  turnActive: boolean;
  pending: number;
  frames: number;
  dropped: number;
}

/**
 * An API call that failed with a status the caller may want to branch on.
 *
 * The status matters more than the message here: 404 from the setup endpoints
 * means "the bootstrap gate rejected you" (deliberately indistinguishable from
 * "already enrolled"), and 429 from login means rate-limited rather than wrong.
 */
export class ApiError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let response: Response;
  try {
    response = await fetch(path, {
      ...init,
      headers: {
        ...(init?.body ? { "Content-Type": "application/json" } : {}),
        ...init?.headers,
      },
      // The cookie is same-origin anyway, but being explicit means a future
      // change to the dev proxy cannot silently drop credentials.
      credentials: "same-origin",
    });
  } catch (cause) {
    // fetch only rejects on network-level failure, which here means the server
    // is down or the dev proxy has no target. Saying so beats "Failed to fetch".
    throw new ApiError(0, `cannot reach the glance server (${String(cause)})`);
  }

  const text = await response.text();
  let body: unknown = null;
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      // A non-JSON body from an API path means something upstream of the
      // handler answered — a proxy error page, most likely.
      body = null;
    }
  }

  if (!response.ok) {
    const message =
      body && typeof body === "object" && "error" in body && typeof body.error === "string"
        ? body.error
        : `request failed with ${response.status}`;
    throw new ApiError(response.status, message);
  }
  return body as T;
}

export const api = {
  status: () => request<Status>("/api/status"),

  /**
   * Both setup calls carry the bootstrap token in the query string, matching
   * `auth.BootstrapToken`, which reads `?token=` before the header.
   */
  setupBegin: (token: string) =>
    request<Enrollment>(`/api/setup/begin?token=${encodeURIComponent(token)}`, {
      method: "POST",
    }),

  setupComplete: (token: string, secret: string, code: string) =>
    request<{ ok: boolean }>(`/api/setup/complete?token=${encodeURIComponent(token)}`, {
      method: "POST",
      body: JSON.stringify({ secret, code }),
    }),

  login: (code: string) =>
    request<{ ok: boolean }>("/api/login", {
      method: "POST",
      body: JSON.stringify({ code }),
    }),

  logout: () => request<{ ok: boolean }>("/api/logout", { method: "POST" }),

  agents: () => request<{ agents: AgentSummary[] | null }>("/api/agents"),
};
