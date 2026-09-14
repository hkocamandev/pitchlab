import type { ReactNode } from "react";

import { ApiError } from "../api/client";

/** Loading, error and empty are three different answers and the brief calls
 * for all three. Collapsing them into one spinner-or-nothing is how a
 * dashboard ends up showing a blank panel and leaving the viewer unsure
 * whether the data is missing or the request failed. */

export function Loading({ what }: { what: string }) {
  return <div className="state">Loading {what}…</div>;
}

export function ErrorState({ error, what }: { error: unknown; what: string }) {
  const message = error instanceof Error ? error.message : String(error);
  const requestId = error instanceof ApiError ? error.problem?.request_id : undefined;

  return (
    <div className="state error">
      <div>Could not load {what}.</div>
      <div className="muted" style={{ marginTop: 6 }}>{message}</div>
      {requestId && (
        // Surfaced so a failure in the browser can be found in the server
        // logs without guessing at timestamps.
        <div className="muted mono" style={{ marginTop: 6, fontSize: 11 }}>
          request {requestId}
        </div>
      )}
    </div>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="state">{children}</div>;
}

/** Query renders the right one of the three states. */
export function Query<T>({
  data,
  isLoading,
  error,
  what,
  isEmpty,
  empty,
  children,
}: {
  data: T | undefined;
  isLoading: boolean;
  error: unknown;
  what: string;
  isEmpty?: (data: T) => boolean;
  empty?: ReactNode;
  children: (data: T) => ReactNode;
}) {
  if (isLoading) return <Loading what={what} />;
  if (error) return <ErrorState error={error} what={what} />;
  if (!data) return <Empty>No {what}.</Empty>;
  if (isEmpty?.(data)) return <Empty>{empty ?? `No ${what} yet.`}</Empty>;
  return <>{children(data)}</>;
}

export function Kpi({
  label,
  value,
  unit,
  digits = 1,
}: {
  label: string;
  value: number | null | undefined;
  unit?: string;
  digits?: number;
}) {
  return (
    <div className="kpi">
      <div className="label">{label}</div>
      <div className="value">
        {/* null is not zero. A pitcher with no spin readings has no average
            spin, and printing 0 rpm would be a measurement nobody made. */}
        {value === null || value === undefined ? (
          <span className="muted">—</span>
        ) : (
          value.toFixed(digits)
        )}
        {unit && value !== null && value !== undefined && <span className="unit">{unit}</span>}
      </div>
    </div>
  );
}

/** SyntheticBadge marks replayed data.
 *
 * Shown wherever a session appears. A viewer has to be able to tell a
 * simulation from live play without reading the source, and the flag travels
 * all the way from the replay simulator's event to make that possible. */
export function SyntheticBadge({ synthetic }: { synthetic: boolean }) {
  if (!synthetic) return null;
  return (
    <span className="badge synthetic" title="Replayed from historical data, not live play">
      synthetic
    </span>
  );
}

export function SeverityBadge({ severity }: { severity: string }) {
  const cls = severity.toLowerCase();
  return <span className={`badge ${cls}`}>{severity}</span>;
}

export function formatDate(iso: string | null | undefined): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toISOString().slice(0, 10);
}

export function formatTime(iso: string | null | undefined): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toISOString().slice(11, 19);
}

export function formatNumber(
  value: number | null | undefined,
  digits = 1,
): string {
  if (value === null || value === undefined) return "—";
  return value.toFixed(digits);
}

/** Outcome labels, shortened for table cells. */
export const OUTCOME_LABELS: Record<string, string> = {
  BALL: "Ball",
  CALLED_STRIKE: "Called strike",
  SWINGING_STRIKE: "Whiff",
  FOUL: "Foul",
  IN_PLAY: "In play",
};

export const OUTCOME_ORDER = [
  "BALL",
  "CALLED_STRIKE",
  "SWINGING_STRIKE",
  "FOUL",
  "IN_PLAY",
] as const;
