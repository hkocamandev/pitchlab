import type { Pitch } from "../api/types.gen";

/** Colours by outcome, so the plot answers "where did the whiffs come from"
 * rather than only "where were the pitches". */
const OUTCOME_COLORS: Record<string, string> = {
  BALL: "#8b98a5",
  CALLED_STRIKE: "#4a9eff",
  SWINGING_STRIKE: "#3fb950",
  FOUL: "#d29922",
  IN_PLAY: "#f85149",
};

// The rulebook zone is 17 inches wide, and the ball's radius extends the
// callable area by about an inch and a half on each side.
const ZONE_HALF_WIDTH = 17 / 24; // feet
const ZONE_BOTTOM = 1.5;
const ZONE_TOP = 3.5;

const VIEW = { xMin: -2.2, xMax: 2.2, zMin: 0, zMax: 5 };
const WIDTH = 260;
const HEIGHT = 300;

function toSvg(x: number, z: number): { cx: number; cy: number } {
  const cx = ((x - VIEW.xMin) / (VIEW.xMax - VIEW.xMin)) * WIDTH;
  // SVG y grows downward and the strike zone does not, so the vertical axis
  // is inverted here rather than in the data.
  const cy = HEIGHT - ((z - VIEW.zMin) / (VIEW.zMax - VIEW.zMin)) * HEIGHT;
  return { cx, cy };
}

/** StrikeZone draws pitch locations from the catcher's view.
 *
 * Raw plate_x is used, not the batter-frame value. The batter-frame column
 * exists so the model can compare inside and outside across stances; a
 * location plot has to show where the pitch physically crossed the plate, and
 * mirroring it for left-handed batters would move pitches that never moved.
 */
export function StrikeZone({
  pitches,
  selectedId,
  onSelect,
}: {
  pitches: Pitch[];
  selectedId?: string | null;
  onSelect?: (id: string) => void;
}) {
  const plotted = pitches.filter(
    (p) => p.measurement?.plate_x !== null && p.measurement?.plate_z !== null,
  );

  if (plotted.length === 0) {
    return <div className="state">No location readings.</div>;
  }

  const zone = {
    left: toSvg(-ZONE_HALF_WIDTH, ZONE_TOP),
    right: toSvg(ZONE_HALF_WIDTH, ZONE_BOTTOM),
  };

  return (
    <div>
      <svg
        width={WIDTH}
        height={HEIGHT}
        viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
        role="img"
        aria-label="Pitch locations from the catcher's view"
        style={{ maxWidth: "100%" }}
      >
        <rect x={0} y={0} width={WIDTH} height={HEIGHT} fill="#1e2632" rx={6} />
        <rect
          x={zone.left.cx}
          y={zone.left.cy}
          width={zone.right.cx - zone.left.cx}
          height={zone.right.cy - zone.left.cy}
          fill="none"
          stroke="#8b98a5"
          strokeWidth={1.5}
        />
        {plotted.map((p) => {
          const { cx, cy } = toSvg(
            p.measurement!.plate_x as number,
            p.measurement!.plate_z as number,
          );
          const selected = p.id === selectedId;
          return (
            <circle
              key={p.id}
              cx={cx}
              cy={cy}
              r={selected ? 7 : 4.5}
              fill={OUTCOME_COLORS[p.actual_outcome ?? ""] ?? "#8b98a5"}
              fillOpacity={selected ? 1 : 0.75}
              stroke={selected ? "#e6edf3" : "none"}
              strokeWidth={selected ? 2 : 0}
              style={{ cursor: onSelect ? "pointer" : "default" }}
              onClick={() => onSelect?.(p.id)}
            >
              <title>
                #{p.session_pitch_index} {p.pitch_type ?? ""} {p.actual_outcome ?? ""}
              </title>
            </circle>
          );
        })}
      </svg>
      <div style={{ display: "flex", gap: 12, flexWrap: "wrap", marginTop: 8, fontSize: 11 }}>
        {Object.entries(OUTCOME_COLORS).map(([outcome, color]) => (
          <span key={outcome} className="muted">
            <span
              style={{
                display: "inline-block",
                width: 8,
                height: 8,
                borderRadius: "50%",
                background: color,
                marginRight: 4,
              }}
            />
            {outcome.replace("_", " ").toLowerCase()}
          </span>
        ))}
      </div>
    </div>
  );
}
