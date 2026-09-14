import { useCallback, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams } from "react-router-dom";
import {
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
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
  SeverityBadge,
  SyntheticBadge,
  formatNumber,
  formatTime,
} from "../components/common";
import { ModelCard } from "../components/ModelCard";
import { ProbabilityBars } from "../components/PitchDetail";
import { useSessionStream } from "../hooks/useSessionStream";
import type { ConnectionStatus } from "../hooks/useSessionStream";
import type { LivePitch } from "../api/types.gen";

const STATUS_LABEL: Record<ConnectionStatus, string> = {
  connecting: "connecting",
  live: "live",
  reconnecting: "reconnecting",
  closed: "closed",
};

function ConnectionIndicator({ status, attempt }: { status: ConnectionStatus; attempt: number }) {
  return (
    <span>
      <span className={`status-dot status-${status}`} />
      {STATUS_LABEL[status]}
      {status === "reconnecting" && attempt > 0 && (
        <span className="muted"> · attempt {attempt}</span>
      )}
    </span>
  );
}

function scoreOf(pitch: LivePitch, variant: string): number | null {
  return pitch.predictions?.find((p) => p.variant === variant)?.score ?? null;
}

/** LiveSession is the page that shows the system is actually live.
 *
 * It follows the connection order the contract describes: a REST snapshot
 * first so the screen is never empty, then the socket. The two are reconciled
 * rather than merged blindly -- the REST read establishes what happened before
 * the connection, and the stream adds what happens after. */
export function LiveSession() {
  const { sessionId = "" } = useParams();
  const queryClient = useQueryClient();
  const [selected, setSelected] = useState<LivePitch | null>(null);

  const session = useQuery({
    queryKey: ["session", sessionId],
    queryFn: () => api.session(sessionId),
    enabled: !!sessionId,
  });

  // The snapshot. Fetched before the socket opens so the page has something to
  // show immediately: at a realistic pace the next pitch is twenty seconds
  // away, and a WebSocket-only page would stare at the viewer until then.
  const snapshot = useQuery({
    queryKey: ["live-snapshot", sessionId],
    queryFn: () =>
      api.sessionPitches(sessionId, {
        limit: 20,
        order: "desc",
        include: "measurement,prediction",
      }),
    enabled: !!sessionId,
  });

  // A sequence gap means the server dropped messages for this connection
  // because it could not keep up. Refetching the snapshot is the compensation
  // the sequence number exists to enable -- without it the timeline would
  // quietly be missing pitches.
  const reconcile = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: ["live-snapshot", sessionId] });
    void queryClient.invalidateQueries({ queryKey: ["session", sessionId] });
  }, [queryClient, sessionId]);

  const stream = useSessionStream(sessionId, {
    onGap: reconcile,
    onReconnect: reconcile,
  });

  // The stream first, then anything from the snapshot it has not superseded.
  const feed = useMemo(() => {
    const seen = new Set(stream.pitches.map((p) => p.pitch_id));
    const fromSnapshot: LivePitch[] = (snapshot.data?.data ?? [])
      .filter((p) => !seen.has(p.id))
      .map((p) => ({
        pitch_id: p.id,
        session_pitch_index: p.session_pitch_index,
        at_bat_number: p.at_bat_number,
        pitch_number: p.pitch_number,
        thrown_at: p.thrown_at,
        context: {
          balls: p.context.balls,
          strikes: p.context.strikes,
          outs_when_up: p.context.outs_when_up,
          inning: p.context.inning,
          on_1b: p.context.on_1b,
          on_2b: p.context.on_2b,
          on_3b: p.context.on_3b,
        },
        stand: p.context.stand,
        p_throws: p.context.p_throws,
        pitch_type: p.pitch_type ?? undefined,
        pitch_name: p.pitch_name ?? undefined,
        measurement: (p.measurement ?? {}) as LivePitch["measurement"],
        predictions: (p.predictions ?? []) as LivePitch["predictions"],
        actual_outcome: p.actual_outcome ?? undefined,
        prediction_status: p.prediction_status,
        is_synthetic: true,
      }));

    return [...stream.pitches, ...fromSnapshot]
      .sort((a, b) => b.session_pitch_index - a.session_pitch_index)
      .slice(0, 40);
  }, [stream.pitches, snapshot.data]);

  const latest = feed[0];

  // Live KPIs: the stream's rollup when one has arrived, the snapshot's
  // totals until then. Both come from the same counters -- one over the socket
  // and one from the REST read -- so they cannot disagree about what happened,
  // only about how recently.
  const pitchCount = stream.metrics?.pitch_count ?? stream.snapshot?.pitchCount ?? 0;
  const speeds = feed
    .map((p) => p.measurement?.release_speed)
    .filter((v): v is number => typeof v === "number");
  const avgSpeed = speeds.length ? speeds.reduce((a, b) => a + b, 0) / speeds.length : null;
  const scores = feed
    .map((p) => scoreOf(p, "pitching"))
    .filter((v): v is number => typeof v === "number");
  const avgScore = scores.length ? scores.reduce((a, b) => a + b, 0) / scores.length : null;
  const whiffs = feed.filter((p) => p.actual_outcome === "SWINGING_STRIKE").length;

  const chartData = [...feed]
    .reverse()
    .map((p) => ({ index: p.session_pitch_index, speed: p.measurement?.release_speed ?? null }));

  if (session.isLoading) return <Loading what="outing" />;
  if (session.error) return <ErrorState error={session.error} what="outing" />;

  const detail = selected ?? latest ?? null;
  const detailPrediction = detail?.predictions?.find((p) => p.variant === "pitching");

  return (
    <>
      <div className="panel">
        <h2>Live</h2>
        <div style={{ display: "flex", alignItems: "baseline", gap: 12, flexWrap: "wrap" }}>
          <div style={{ fontSize: 20, fontWeight: 600 }}>
            {session.data?.pitcher ? (
              <Link to={`/athletes/${session.data.pitcher.id}`}>
                {session.data.pitcher.full_name}
              </Link>
            ) : (
              "Outing"
            )}
          </div>
          <ConnectionIndicator status={stream.status} attempt={stream.attempt} />
          <SyntheticBadge synthetic={session.data?.is_synthetic ?? false} />
          <Link to={`/sessions/${sessionId}`}>full detail →</Link>
        </div>

        {stream.missed > 0 && (
          // Surfaced rather than hidden. The viewer is told the feed skipped
          // and that the gap has been filled from the API, which is more
          // honest than a timeline that silently has holes in it.
          <div className="muted" style={{ marginTop: 8 }}>
            {stream.missed} message{stream.missed === 1 ? "" : "s"} were dropped for this
            connection and have been refetched.
          </div>
        )}
        {stream.error && (
          <div className="muted" style={{ marginTop: 8 }}>
            {stream.error}
          </div>
        )}
        {stream.closedReason && (
          <div className="muted" style={{ marginTop: 8 }}>
            Outing ended ({stream.closedReason.toLowerCase()}).
          </div>
        )}

        <div className="kpis" style={{ marginTop: 16 }}>
          <Kpi label="Pitches" value={pitchCount} digits={0} />
          <Kpi label="Recent velocity" value={avgSpeed} unit="mph" />
          <Kpi label="Recent PitchScore" value={avgScore} />
          <Kpi label="Whiffs shown" value={whiffs} digits={0} />
        </div>
      </div>

      {stream.anomalies.length > 0 && (
        <div className="panel" style={{ borderColor: "var(--warn)" }}>
          <h2>Anomaly detected</h2>
          {stream.anomalies.map((a) => (
            <div key={a.anomaly_id} style={{ marginBottom: 6 }}>
              <SeverityBadge severity={a.severity} />{" "}
              <span className="mono">{a.metric}</span>{" "}
              {a.pitch_type && <span className="muted mono">{a.pitch_type} </span>}
              {formatNumber(a.observed_value)} vs baseline {formatNumber(a.baseline_median)} ·{" "}
              robust z {formatNumber(a.z_robust, 2)} · {a.direction.toLowerCase()}
            </div>
          ))}
        </div>
      )}

      <div className="grid sidebar">
        <div>
          <div className="panel">
            <h2>Velocity, rolling window</h2>
            {chartData.length > 1 ? (
              <ResponsiveContainer width="100%" height={160}>
                <LineChart data={chartData} margin={{ top: 8, right: 8, bottom: 0, left: -18 }}>
                  <CartesianGrid stroke="#2a3441" strokeDasharray="2 4" />
                  <XAxis dataKey="index" tick={{ stroke: "#8b98a5", fontSize: 11 }} tickLine={false} />
                  <YAxis
                    tick={{ stroke: "#8b98a5", fontSize: 11 }}
                    tickLine={false}
                    domain={["dataMin - 2", "dataMax + 2"]}
                  />
                  <Line type="monotone" dataKey="speed" stroke="#4a9eff" dot={false} isAnimationActive={false} />
                </LineChart>
              </ResponsiveContainer>
            ) : (
              <Empty>Waiting for pitches.</Empty>
            )}
          </div>

          <div className="panel">
            <h2>Feed</h2>
            {snapshot.isLoading && feed.length === 0 && <Loading what="recent pitches" />}
            {feed.length === 0 && !snapshot.isLoading ? (
              <Empty>
                No pitches yet. The feed fills as the outing progresses — start a replay to see
                it move.
              </Empty>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th>#</th>
                    <th>Time</th>
                    <th>Count</th>
                    <th>Type</th>
                    <th className="num">mph</th>
                    <th>Outcome</th>
                    <th className="num">Score</th>
                  </tr>
                </thead>
                <tbody>
                  {feed.map((p) => (
                    <tr
                      key={p.pitch_id}
                      className={`clickable feed-row${detail?.pitch_id === p.pitch_id ? " selected" : ""}`}
                      onClick={() => setSelected(p)}
                    >
                      <td className="num">{p.session_pitch_index}</td>
                      <td className="mono">{formatTime(p.thrown_at)}</td>
                      <td className="num">
                        {p.context.balls}-{p.context.strikes}
                      </td>
                      <td className="mono">{p.pitch_type ?? "—"}</td>
                      <td className="num">{formatNumber(p.measurement?.release_speed)}</td>
                      <td>{OUTCOME_LABELS[p.actual_outcome ?? ""] ?? "—"}</td>
                      <td className="num">{formatNumber(scoreOf(p, "pitching"))}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        </div>

        <div>
          <div className="panel">
            <h2>Game state</h2>
            {latest ? (
              <table>
                <tbody>
                  <tr>
                    <td className="muted">Inning</td>
                    <td className="num">{latest.context.inning}</td>
                  </tr>
                  <tr>
                    <td className="muted">Count</td>
                    <td className="num">
                      {latest.context.balls}-{latest.context.strikes}
                    </td>
                  </tr>
                  <tr>
                    <td className="muted">Outs</td>
                    <td className="num">{latest.context.outs_when_up}</td>
                  </tr>
                  <tr>
                    <td className="muted">Runners</td>
                    <td className="num">
                      {[
                        latest.context.on_1b && "1B",
                        latest.context.on_2b && "2B",
                        latest.context.on_3b && "3B",
                      ]
                        .filter(Boolean)
                        .join(" ") || "empty"}
                    </td>
                  </tr>
                  <tr>
                    <td className="muted">Batter stance</td>
                    <td className="num">{latest.stand}</td>
                  </tr>
                </tbody>
              </table>
            ) : (
              <Empty>No pitches yet.</Empty>
            )}
          </div>

          {detail && (
            <div className="panel">
              <h2>
                Pitch #{detail.session_pitch_index} ·{" "}
                {detail.pitch_name ?? detail.pitch_type ?? "unknown"}
              </h2>
              {detailPrediction?.probabilities ? (
                <ProbabilityBars
                  probabilities={detailPrediction.probabilities}
                  actual={detail.actual_outcome ?? null}
                />
              ) : (
                <div className="muted">
                  No prediction for this pitch ({detail.prediction_status.toLowerCase()}).
                </div>
              )}
              <div className="muted" style={{ marginTop: 10, fontSize: 12 }}>
                {formatNumber(detail.measurement?.release_speed)} mph ·{" "}
                {formatNumber(detail.measurement?.release_spin_rate, 0)} rpm · arm-side break{" "}
                {formatNumber(detail.measurement?.pfx_x_arm, 2)} ft
              </div>
            </div>
          )}
        </div>
      </div>

      <ModelCard />
    </>
  );
}
