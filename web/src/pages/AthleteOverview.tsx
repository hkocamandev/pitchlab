import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router-dom";

import { api } from "../api/client";
import { MovementProfile, PitchTypeBars, TrendChart } from "../components/charts";
import {
  Empty,
  ErrorState,
  Kpi,
  Loading,
  SeverityBadge,
  SyntheticBadge,
  formatDate,
  formatNumber,
} from "../components/common";
import { ModelCard } from "../components/ModelCard";

export function AthleteOverview() {
  const { athleteId = "" } = useParams();

  const athlete = useQuery({
    queryKey: ["athlete", athleteId],
    queryFn: () => api.athlete(athleteId),
    enabled: !!athleteId,
  });

  const analytics = useQuery({
    queryKey: ["analytics", athleteId],
    queryFn: () => api.analytics(athleteId, { include: "distribution,trend" }),
    enabled: !!athleteId,
  });

  const sessions = useQuery({
    queryKey: ["athlete-sessions", athleteId],
    queryFn: () => api.athleteSessions(athleteId, { limit: 15 }),
    enabled: !!athleteId,
  });

  const anomalies = useQuery({
    queryKey: ["athlete-anomalies", athleteId],
    queryFn: () => api.athleteAnomalies(athleteId, { limit: 10 }),
    enabled: !!athleteId,
  });

  if (athlete.isLoading) return <Loading what="athlete" />;
  if (athlete.error) return <ErrorState error={athlete.error} what="athlete" />;
  if (!athlete.data) return <Empty>No athlete.</Empty>;

  const a = athlete.data;
  const summary = analytics.data?.summary;

  return (
    <>
      <div className="panel">
        <h2>Pitcher</h2>
        <div style={{ display: "flex", alignItems: "baseline", gap: 12, flexWrap: "wrap" }}>
          <div style={{ fontSize: 22, fontWeight: 600 }}>{a.full_name}</div>
          <div className="muted">
            throws {a.throws ?? "—"} · MLBAM {a.mlbam_id} · {a.totals.session_count} outings ·{" "}
            {a.totals.pitch_count} pitches
          </div>
        </div>
      </div>

      <div className="panel">
        <h2>Summary</h2>
        {analytics.isLoading && <Loading what="analytics" />}
        {analytics.error && <ErrorState error={analytics.error} what="analytics" />}
        {summary && (
          <div className="kpis">
            <Kpi label="Pitches" value={summary.pitch_count} digits={0} />
            <Kpi label="Avg velocity" value={summary.avg_release_speed} unit="mph" />
            <Kpi label="Avg spin" value={summary.avg_release_spin_rate} unit="rpm" digits={0} />
            <Kpi label="Avg extension" value={summary.avg_release_extension} unit="ft" digits={2} />
            <Kpi label="PitchScore" value={summary.avg_pitch_score} />
            <Kpi
              label="Whiff rate"
              value={summary.whiff_rate === null ? null : summary.whiff_rate * 100}
              unit="%"
            />
            <Kpi
              label="In zone"
              value={summary.in_zone_rate === null ? null : summary.in_zone_rate * 100}
              unit="%"
            />
            <Kpi
              label="Open anomalies"
              value={analytics.data?.open_anomaly_count ?? null}
              digits={0}
            />
          </div>
        )}
      </div>

      <div className="grid cols-2">
        <div className="panel">
          <h2>Arsenal</h2>
          {analytics.data?.pitch_type_distribution?.length ? (
            <PitchTypeBars stats={analytics.data.pitch_type_distribution} />
          ) : (
            <Empty>No pitches recorded yet.</Empty>
          )}
        </div>

        <div className="panel">
          <h2>Movement profile · arm-side vs induced vertical break</h2>
          {analytics.data?.pitch_type_distribution?.length ? (
            <MovementProfile stats={analytics.data.pitch_type_distribution} />
          ) : (
            <Empty>No movement readings yet.</Empty>
          )}
        </div>
      </div>

      <div className="panel">
        <h2>Trend by outing · velocity and PitchScore</h2>
        {analytics.data?.trend?.length ? (
          <TrendChart points={analytics.data.trend} />
        ) : (
          <Empty>Not enough outings for a trend.</Empty>
        )}
      </div>

      <div className="grid sidebar">
        <div className="panel">
          <h2>Outings</h2>
          {sessions.isLoading && <Loading what="outings" />}
          {sessions.error && <ErrorState error={sessions.error} what="outings" />}
          {sessions.data &&
            (sessions.data.data.length === 0 ? (
              <Empty>No outings yet.</Empty>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th>Date</th>
                    <th>Opponent</th>
                    <th className="num">Pitches</th>
                    <th className="num">Avg mph</th>
                    <th className="num">PitchScore</th>
                    <th>Status</th>
                  </tr>
                </thead>
                <tbody>
                  {sessions.data.data.map((s) => (
                    <tr key={s.id}>
                      <td>
                        <Link to={`/sessions/${s.id}`}>
                          {formatDate(s.source_game_date ?? s.started_at)}
                        </Link>
                      </td>
                      <td>
                        {s.opponent_team ?? "—"} <SyntheticBadge synthetic={s.is_synthetic} />
                      </td>
                      <td className="num">{s.metrics.pitch_count}</td>
                      <td className="num">{formatNumber(s.metrics.avg_release_speed)}</td>
                      <td className="num">{formatNumber(s.metrics.avg_pitch_score)}</td>
                      <td>
                        {s.status === "ACTIVE" ? (
                          <Link to={`/live/${s.id}`}>live</Link>
                        ) : (
                          <span className="muted">{s.status.toLowerCase()}</span>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ))}
        </div>

        <div className="panel">
          <h2>Anomalies</h2>
          {anomalies.isLoading && <Loading what="anomalies" />}
          {anomalies.error && <ErrorState error={anomalies.error} what="anomalies" />}
          {anomalies.data &&
            (anomalies.data.data.length === 0 ? (
              // An empty anomaly list is good news, and saying so is more
              // useful than an empty panel that reads like a failure.
              <Empty>No deviations from this pitcher's baseline.</Empty>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th>Metric</th>
                    <th className="num">z</th>
                    <th>Severity</th>
                  </tr>
                </thead>
                <tbody>
                  {anomalies.data.data.map((an) => (
                    <tr key={an.id}>
                      <td>
                        <div>
                          {an.metric} <span className="muted mono">{an.pitch_type ?? ""}</span>
                        </div>
                        <div className="muted" style={{ fontSize: 12 }}>
                          {formatNumber(an.observed_value)} vs baseline{" "}
                          {formatNumber(an.baseline_median)} · {an.direction.toLowerCase()}
                        </div>
                      </td>
                      <td className="num">{formatNumber(an.z_robust, 2)}</td>
                      <td>
                        <SeverityBadge severity={an.severity} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ))}
        </div>
      </div>

      <ModelCard />
    </>
  );
}
