import type {
  Analytics,
  Anomaly,
  AthleteDetail,
  AthleteListItem,
  ModelVersion,
  Page,
  Pitch,
  Session,
  SessionDetail,
} from "./types.gen";

// List is the envelope every collection endpoint uses. It is declared here
// rather than generated because it is a Go generic, and a generic has no
// single instantiation to reflect over.
export interface List<T> {
  data: T[];
  page: Page;
}

// Problem is RFC 7807. The API returns it for every failure, so the client has
// exactly one error shape to understand.
export interface Problem {
  type: string;
  title: string;
  status: number;
  detail?: string;
  instance?: string;
  request_id?: string;
  errors?: { field: string; message: string }[];
}

export class ApiError extends Error {
  readonly status: number;
  readonly problem: Problem | null;

  constructor(status: number, problem: Problem | null, fallback: string) {
    super(problem?.detail || problem?.title || fallback);
    this.name = "ApiError";
    this.status = status;
    this.problem = problem;
  }

  /** notFound separates "this does not exist" from "something went wrong",
   * which the pages render very differently. */
  get notFound(): boolean {
    return this.status === 404;
  }
}

/** unauthorized is called whenever the API says the session is gone.
 *
 * One place, rather than a 401 branch in every page. The session can expire
 * between two requests -- the token lasts a day, and a dashboard left open
 * overnight will outlive it -- and without this the app would sit there
 * rendering errors instead of asking the person to sign in again. */
let unauthorized: (() => void) | null = null;

export function onUnauthorized(handler: (() => void) | null): void {
  unauthorized = handler;
}

interface RequestOptions {
  method?: string;
  body?: unknown;
  signal?: AbortSignal;
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const response = await fetch(path, {
    method: options.method ?? "GET",
    signal: options.signal,
    // The session is an httpOnly cookie, so the credential is attached by the
    // browser and is never readable from JavaScript -- which is the point:
    // script that cannot read a token cannot exfiltrate one.
    credentials: "same-origin",
    headers: options.body
      ? { Accept: "application/json", "Content-Type": "application/json" }
      : { Accept: "application/json" },
    body: options.body ? JSON.stringify(options.body) : undefined,
  });

  if (!response.ok) {
    // The body is a problem document unless something upstream of the API
    // answered, which is exactly when parsing it would throw and hide the real
    // status behind a JSON error.
    let problem: Problem | null = null;
    try {
      problem = (await response.json()) as Problem;
    } catch {
      problem = null;
    }

    // The login endpoint answers 401 for a wrong password, and that must not
    // be read as "the session ended" -- it would clear a session that was
    // never established and hide the message the person needs to see.
    if (response.status === 401 && !path.startsWith("/api/v1/auth/")) {
      unauthorized?.();
    }

    throw new ApiError(response.status, problem, response.statusText);
  }

  return (await response.json()) as T;
}

function query(params: Record<string, string | number | undefined | null>): string {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== null && value !== "") {
      search.set(key, String(value));
    }
  }
  const encoded = search.toString();
  return encoded ? `?${encoded}` : "";
}

/** User is the signed-in identity. It deliberately carries no token: there is
 * nothing for the client to hold, because the credential lives in a cookie it
 * cannot read. */
export interface User {
  id: string;
  username: string;
  display_name: string;
  role: string;
}

export interface AuthSession {
  user: User;
  expires_at: string;
}

export const api = {
  login(username: string, password: string) {
    return request<AuthSession>("/api/v1/auth/login", {
      method: "POST",
      body: { username, password },
    });
  },

  logout() {
    return request<{ status: string }>("/api/v1/auth/logout", { method: "POST" });
  },

  /** me is how the app decides between the login page and the dashboard on
   * load. It cannot read the cookie to find out, so it asks. */
  me(signal?: AbortSignal) {
    return request<AuthSession>("/api/v1/auth/me", { signal });
  },

  athletes(params: { limit?: number; role?: string; q?: string } = {}) {
    return request<List<AthleteListItem>>(`/api/v1/athletes${query(params)}`);
  },

  athlete(id: string) {
    return request<AthleteDetail>(`/api/v1/athletes/${id}`);
  },

  analytics(id: string, params: { include?: string; from?: string; to?: string } = {}) {
    return request<Analytics>(`/api/v1/athletes/${id}/analytics${query(params)}`);
  },

  athleteSessions(id: string, params: { limit?: number; status?: string } = {}) {
    return request<List<Session>>(`/api/v1/athletes/${id}/sessions${query(params)}`);
  },

  athleteAnomalies(id: string, params: { limit?: number; status?: string } = {}) {
    return request<List<Anomaly>>(`/api/v1/athletes/${id}/anomalies${query(params)}`);
  },

  activeSessions(params: { limit?: number } = {}) {
    return request<List<Session>>(
      `/api/v1/sessions${query({ status: "ACTIVE", ...params })}`,
    );
  },

  session(id: string) {
    return request<SessionDetail>(`/api/v1/sessions/${id}`);
  },

  sessionPitches(
    id: string,
    params: { limit?: number; order?: "asc" | "desc"; cursor?: string; include?: string } = {},
  ) {
    return request<List<Pitch>>(`/api/v1/sessions/${id}/pitches${query(params)}`);
  },

  pitchPredictions(id: string) {
    return request<{ data: unknown[] }>(`/api/v1/pitches/${id}/predictions`);
  },

  models() {
    return request<{ data: ModelVersion[] }>("/api/v1/models");
  },
};
