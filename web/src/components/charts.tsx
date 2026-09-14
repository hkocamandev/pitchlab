import {
  CartesianGrid,
  Line,
  LineChart,
  ReferenceLine,
  ResponsiveContainer,
  Scatter,
  ScatterChart,
  Tooltip,
  XAxis,
  YAxis,
  ZAxis,
} from "recharts";

import type { PitchTypeStat, TrendPoint } from "../api/types.gen";
import { formatDate, formatNumber } from "./common";

const AXIS = { stroke: "#8b98a5", fontSize: 11 };
const GRID = "#2a3441";

/** PitchTypeBars shows arsenal usage.
 *
 * A horizontal bar per pitch type rather than a pie: usage shares are read as
 * comparisons, and the numbers that matter alongside them -- velocity, spin,
 * score -- fit next to a bar and not inside a slice. */
export function PitchTypeBars({ stats }: { stats: PitchTypeStat[] }) {
  const sorted = [...stats].sort((a, b) => b.count - a.count);

  return (
    <div>
      {sorted.map((s) => (
        <div className="bar-row" key={s.pitch_type}>
          <div className="name">
            <span className="mono">{s.pitch_type}</span>{" "}
            <span className="muted">{s.pitch_name ?? ""}</span>
          </div>
          <div className="bar-track">
            <div className="bar-fill" style={{ width: `${Math.round(s.usage_pct * 100)}%` }} />
          </div>
          <div className="stat num">
            {(s.usage_pct * 100).toFixed(1)}% · {formatNumber(s.avg_release_speed)} mph ·{" "}
            {formatNumber(s.avg_release_spin_rate, 0)} rpm · {formatNumber(s.avg_pitch_score)}
          </div>
        </div>
      ))}
    </div>
  );
}

/** TrendChart plots velocity and score per outing on two axes.
 *
 * Two axes because the quantities are unrelated in scale -- 93 mph against a
 * score centred on 100 -- and forcing them onto one would flatten whichever
 * moves less. */
export function TrendChart({ points }: { points: TrendPoint[] }) {
  // Oldest first: a trend read left to right has to move forward in time.
  const data = [...points]
    .sort((a, b) => a.started_at.localeCompare(b.started_at))
    .map((p) => ({
      date: formatDate(p.source_game_date ?? p.started_at),
      speed: p.avg_release_speed,
      score: p.avg_pitch_score,
      pitches: p.pitch_count,
    }));

  return (
    <ResponsiveContainer width="100%" height={220}>
      <LineChart data={data} margin={{ top: 8, right: 8, bottom: 0, left: -18 }}>
        <CartesianGrid stroke={GRID} strokeDasharray="2 4" />
        <XAxis dataKey="date" tick={AXIS} tickLine={false} axisLine={{ stroke: GRID }} />
        <YAxis
          yAxisId="speed"
          tick={AXIS}
          tickLine={false}
          axisLine={{ stroke: GRID }}
          domain={["dataMin - 1", "dataMax + 1"]}
        />
        <YAxis
          yAxisId="score"
          orientation="right"
          tick={AXIS}
          tickLine={false}
          axisLine={{ stroke: GRID }}
          domain={["dataMin - 2", "dataMax + 2"]}
        />
        <Tooltip contentStyle={{ background: "#171d26", border: "1px solid #2a3441" }} />
        {/* 100 is the league-average PitchScore by construction, so it is the
            line a coach actually compares against. */}
        <ReferenceLine yAxisId="score" y={100} stroke="#8b98a5" strokeDasharray="3 3" />
        <Line
          yAxisId="speed"
          type="monotone"
          dataKey="speed"
          stroke="#4a9eff"
          dot={false}
          name="avg mph"
        />
        <Line
          yAxisId="score"
          type="monotone"
          dataKey="score"
          stroke="#3fb950"
          dot={false}
          name="PitchScore"
        />
      </LineChart>
    </ResponsiveContainer>
  );
}

/** MovementProfile plots each pitch type's average break.
 *
 * This is the plot a pitching coach looks at first, and it is where the domain
 * research shows up in the interface most directly.
 *
 * The horizontal axis is arm-side movement, not raw pfx_x. Raw horizontal
 * break has the opposite sign for a left-hander, so plotting it would put two
 * pitchers with identical arsenals in mirrored quadrants. The normalization
 * that makes them comparable happens in the stream processor; this chart just
 * has to use the right column.
 *
 * Centroids per pitch type rather than one point per pitch: the per-type
 * averages are already computed and cached server-side, and a scatter of
 * thousands of individual pitches is a cloud rather than a profile. */
export function MovementProfile({ stats }: { stats: PitchTypeStat[] }) {
  const points = stats
    .filter((s) => s.avg_pfx_x_arm !== null && s.avg_pfx_z !== null)
    .map((s) => ({
      x: (s.avg_pfx_x_arm as number) * 12,
      z: (s.avg_pfx_z as number) * 12,
      count: s.count,
      label: s.pitch_type,
    }));

  if (points.length === 0) {
    return <div className="state">No movement readings.</div>;
  }

  return (
    <ResponsiveContainer width="100%" height={260}>
      <ScatterChart margin={{ top: 8, right: 16, bottom: 4, left: -18 }}>
        <CartesianGrid stroke={GRID} strokeDasharray="2 4" />
        <XAxis
          type="number"
          dataKey="x"
          name="arm-side break"
          unit=" in"
          tick={AXIS}
          tickLine={false}
          axisLine={{ stroke: GRID }}
          domain={[-24, 24]}
        />
        <YAxis
          type="number"
          dataKey="z"
          name="induced vertical break"
          unit=" in"
          tick={AXIS}
          tickLine={false}
          axisLine={{ stroke: GRID }}
          domain={[-24, 24]}
        />
        <ZAxis type="number" dataKey="count" range={[60, 400]} name="pitches" />
        {/* Origin lines: the quadrant a pitch sits in is the reading, so the
            axes have to be visible even when no point is near them. */}
        <ReferenceLine x={0} stroke="#8b98a5" strokeDasharray="3 3" />
        <ReferenceLine y={0} stroke="#8b98a5" strokeDasharray="3 3" />
        <Tooltip
          cursor={{ strokeDasharray: "3 3" }}
          contentStyle={{ background: "#171d26", border: "1px solid #2a3441" }}
          formatter={(value: number | string, name: string) =>
            typeof value === "number" ? [value.toFixed(1), name] : [value, name]
          }
          labelFormatter={() => ""}
        />
        <Scatter data={points} fill="#4a9eff" />
      </ScatterChart>
    </ResponsiveContainer>
  );
}
