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

async function request<T>(path: string, signal?: AbortSignal): Promise<T> {
  const response = await fetch(path, {
    signal,
    headers: { Accept: "application/json" },
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

export const api = {
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
