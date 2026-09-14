import { useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router-dom";
import {
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";

import { api } from "../api/client";
import {
  Empty,
  ErrorState,
  Kpi,
  Loading,
  OUTCOME_LABELS,
  SyntheticBadge,
  formatDate,
  formatNumber,
} from "../components/common";
import { ModelCard } from "../components/ModelCard";
import { PitchDetail } from "../components/PitchDetail";
import { StrikeZone } from "../components/StrikeZone";
import type { Pitch } from "../api/types.gen";

const ROW_HEIGHT = 30;
const VISIBLE_ROWS = 18;
const OVERSCAN = 6;

/** PitchTimeline windows the rows it renders.
 *
 * An outing is a hundred-odd pitches today, but the list is the component that
 * re-renders on every live event, and rendering rows nobody is looking at
 * makes that cost grow with the length of the outing rather than with the
 * height of the window. Windowing is hand-written rather than pulled in from a
 * library: it is about thirty lines for a fixed row height, and the dependency
 * would be larger than the code. */
function PitchTimeline({
  pitches,
  selectedId,
  onSelect,
}: {
  pitches: Pitch[];
  selectedId: string | null;
  onSelect: (id: string) => void;
}) {
  const [scrollTop, setScrollTop] = useState(0);
  const viewportRef = useRef<HTMLDivElement>(null);

  const first = Math.max(0, Math.floor(scrollTop / ROW_HEIGHT) - OVERSCAN);
  const last = Math.min(pitches.length, first + VISIBLE_ROWS + OVERSCAN * 2);
  const visible = pitches.slice(first, last);

  return (
    <div>
      <table>
        <thead>
          <tr>
            <th>#</th>
            <th>Count</th>
            <th>Type</th>
            <th className="num">mph</th>
            <th className="num">rpm</th>
            <th>Outcome</th>
            <th className="num">Score</th>
          </tr>
        </thead>
      </table>
      <div
        ref={viewportRef}
        onScroll={(e) => setScrollTop(e.currentTarget.scrollTop)}
        style={{ height: VISIBLE_ROWS * ROW_HEIGHT, overflowY: "auto" }}
        data-testid="pitch-timeline"
      >
        <div style={{ height: pitches.length * ROW_HEIGHT, position: "relative" }}>
          <table style={{ position: "absolute", top: first * ROW_HEIGHT, width: "100%" }}>
            <tbody>
              {visible.map((p) => {
                const score = p.predictions?.find((x) => x.variant === "pitching")?.score;
                return (
                  <tr
                    key={p.id}
                    className={`clickable${p.id === selectedId ? " selected" : ""}`}
                    style={{ height: ROW_HEIGHT }}
                    onClick={() => onSelect(p.id)}
                  >
                    <td className="num">{p.session_pitch_index}</td>
                    <td className="num">
                      {p.context.balls}-{p.context.strikes}
                    </td>
                    <td className="mono">{p.pitch_type ?? "—"}</td>
                    <td className="num">{formatNumber(p.measurement?.release_speed)}</td>
                    <td className="num">{formatNumber(p.measurement?.release_spin_rate, 0)}</td>
                    <td>{OUTCOME_LABELS[p.actual_outcome ?? ""] ?? "—"}</td>
                    <td className="num">{formatNumber(score)}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  );
}

/** FatigueChart plots velocity and spin against pitch order.
 *
 * Against the sequence index rather than the clock: what a coach is looking
 * for is whether the arm is slowing down over the outing, and under replay the
 * wall clock is compressed by whatever speed the simulator ran at. */
function FatigueChart({ pitches }: { pitches: Pitch[] }) {
  const data = pitches.map((p) => ({
    index: p.session_pitch_index,
    speed: p.measurement?.release_speed ?? null,
    spin: p.measurement?.release_spin_rate ?? null,
  }));

  return (
    <ResponsiveContainer width="100%" height={200}>
      <LineChart data={data} margin={{ top: 8, right: 8, bottom: 0, left: -18 }}>
        <CartesianGrid stroke="#2a3441" strokeDasharray="2 4" />
        <XAxis dataKey="index" tick={{ stroke: "#8b98a5", fontSize: 11 }} tickLine={false} />
        <YAxis
          yAxisId="speed"
          tick={{ stroke: "#8b98a5", fontSize: 11 }}
          tickLine={false}
          domain={["dataMin - 2", "dataMax + 2"]}
        />
        <YAxis
          yAxisId="spin"
          orientation="right"
          tick={{ stroke: "#8b98a5", fontSize: 11 }}
          tickLine={false}
          domain={["dataMin - 100", "dataMax + 100"]}
        />
        <Tooltip contentStyle={{ background: "#171d26", border: "1px solid #2a3441" }} />
        <Line yAxisId="speed" type="monotone" dataKey="speed" stroke="#4a9eff" dot={false} name="mph" />
        <Line yAxisId="spin" type="monotone" dataKey="spin" stroke="#a371f7" dot={false} name="rpm" />
      </LineChart>
    </ResponsiveContainer>
  );
}

export function SessionDetail() {
  const { sessionId = "" } = useParams();
  const [selectedId, setSelectedId] = useState<string | null>(null);

  const session = useQuery({
    queryKey: ["session", sessionId],
    queryFn: () => api.session(sessionId),
    enabled: !!sessionId,
  });

  const pitches = useQuery({
    queryKey: ["session-pitches", sessionId],
    queryFn: () =>
      api.sessionPitches(sessionId, {
        limit: 500,
        order: "asc",
        include: "measurement,prediction",
      }),
    enabled: !!sessionId,
  });

  const rows = useMemo(() => pitches.data?.data ?? [], [pitches.data]);
  const selected = rows.find((p) => p.id === selectedId) ?? rows[rows.length - 1] ?? null;

  if (session.isLoading) return <Loading what="outing" />;
  if (session.error) return <ErrorState error={session.error} what="outing" />;
  if (!session.data) return <Empty>No outing.</Empty>;

  const s = session.data;

  return (
    <>
      <div className="panel">
        <h2>Outing</h2>
        <div style={{ display: "flex", alignItems: "baseline", gap: 12, flexWrap: "wrap" }}>
          <div style={{ fontSize: 20, fontWeight: 600 }}>
            {s.pitcher ? (
              <Link to={`/athletes/${s.pitcher.id}`}>{s.pitcher.full_name}</Link>
            ) : (
              "Unknown pitcher"
            )}
          </div>
          <div className="muted">
            {formatDate(s.source_game_date ?? s.started_at)} · {s.opponent_team ?? "—"} ·{" "}
            {s.status.toLowerCase()}
          </div>
          <SyntheticBadge synthetic={s.is_synthetic} />
          {s.status === "ACTIVE" && <Link to={`/live/${s.id}`}>watch live →</Link>}
        </div>
        <div className="kpis" style={{ marginTop: 16 }}>
          <Kpi label="Pitches" value={s.metrics.pitch_count} digits={0} />
          <Kpi label="Avg velocity" value={s.metrics.avg_release_speed} unit="mph" />
          <Kpi label="Avg spin" value={s.metrics.avg_release_spin_rate} unit="rpm" digits={0} />
          <Kpi label="PitchScore" value={s.metrics.avg_pitch_score} />
          <Kpi label="Anomalies" value={s.anomaly_count} digits={0} />
        </div>
      </div>

      {pitches.isLoading && <Loading what="pitches" />}
      {pitches.error && <ErrorState error={pitches.error} what="pitches" />}
      {pitches.data && rows.length === 0 && <Empty>No pitches recorded for this outing.</Empty>}

      {rows.length > 0 && (
        <>
          <div className="panel">
            <h2>Velocity and spin by pitch number</h2>
            <FatigueChart pitches={rows} />
          </div>

          <div className="grid sidebar">
            <div>
              <div className="panel">
                <h2>Timeline</h2>
                <PitchTimeline pitches={rows} selectedId={selected?.id ?? null} onSelect={setSelectedId} />
              </div>
              <div className="panel">
                <h2>Locations · catcher's view</h2>
                <StrikeZone
                  pitches={rows}
                  selectedId={selected?.id ?? null}
                  onSelect={setSelectedId}
                />
              </div>
            </div>

            <div className="panel">
              <h2>Pitch detail</h2>
              {selected ? <PitchDetail pitch={selected} /> : <Empty>Select a pitch.</Empty>}
            </div>
          </div>
        </>
      )}

      <ModelCard />
    </>
  );
}
