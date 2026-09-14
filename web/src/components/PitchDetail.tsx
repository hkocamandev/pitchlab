import { useState } from "react";

import type { Explanation as ExplanationDTO, Measurement, Pitch, Prediction } from "../api/types.gen";
import { OUTCOME_LABELS, OUTCOME_ORDER, formatNumber } from "./common";

/** ProbabilityBars puts the model's distribution next to what happened.
 *
 * Showing the actual outcome alongside the prediction is deliberate. A
 * dashboard that displayed only the predicted class would present the model as
 * more certain than it is; showing the pitches it got wrong is the honest
 * version, and it is the same scepticism the model evaluation was written
 * with. */
export function ProbabilityBars({
  probabilities,
  actual,
}: {
  probabilities: Record<string, number>;
  actual: string | null | undefined;
}) {
  return (
    <div>
      {OUTCOME_ORDER.map((cls) => {
        const p = probabilities[cls] ?? 0;
        const isActual = actual === cls;
        return (
          <div className={`prob-row${isActual ? " actual" : ""}`} key={cls}>
            <div className="name">
              {OUTCOME_LABELS[cls] ?? cls}
              {isActual && <span className="muted"> ← actual</span>}
            </div>
            <div className="track">
              <div className="fill" style={{ width: `${Math.round(p * 100)}%` }} />
            </div>
            <div className="pct num">{(p * 100).toFixed(1)}%</div>
          </div>
        );
      })}
    </div>
  );
}

function MeasurementRow({
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
    <tr>
      <td className="muted">{label}</td>
      <td className="num">
        {formatNumber(value, digits)}
        {unit && value !== null && value !== undefined && <span className="muted"> {unit}</span>}
      </td>
    </tr>
  );
}

export function MeasurementTable({ m }: { m: Measurement }) {
  return (
    <table>
      <tbody>
        <MeasurementRow label="Velocity" value={m.release_speed} unit="mph" />
        <MeasurementRow label="Spin rate" value={m.release_spin_rate} unit="rpm" digits={0} />
        <MeasurementRow label="Spin axis" value={m.spin_axis} unit="°" digits={0} />
        <MeasurementRow label="Extension" value={m.release_extension} unit="ft" digits={2} />
        <MeasurementRow label="Arm angle" value={m.arm_angle} unit="°" digits={0} />
        {/* Arm-frame values are shown next to the raw ones rather than
            instead of them. The raw reading is what the device produced; the
            arm-frame value is what makes left- and right-handers comparable,
            and hiding the difference would hide the normalization. */}
        <MeasurementRow label="Horizontal break (raw)" value={m.pfx_x} unit="ft" digits={2} />
        <MeasurementRow label="Horizontal break (arm-side)" value={m.pfx_x_arm} unit="ft" digits={2} />
        <MeasurementRow label="Vertical break" value={m.pfx_z} unit="ft" digits={2} />
        <MeasurementRow label="Plate side (raw)" value={m.plate_x} unit="ft" digits={2} />
        <MeasurementRow label="Plate side (batter frame)" value={m.plate_x_bat} unit="ft" digits={2} />
        <MeasurementRow label="Plate height" value={m.plate_z} unit="ft" digits={2} />
        <MeasurementRow label="Zone height (normalized)" value={m.zone_height_norm} digits={2} />
        <tr>
          <td className="muted">In zone</td>
          <td className="num">{m.in_zone === null ? "—" : m.in_zone ? "yes" : "no"}</td>
        </tr>
        <tr>
          <td className="muted">Quality</td>
          <td className="num mono">{m.measurement_quality}</td>
        </tr>
      </tbody>
    </table>
  );
}

function ScoreLine({ p }: { p: Prediction }) {
  return (
    <div style={{ marginBottom: 6 }}>
      <span className="mono">{p.variant}</span>{" "}
      <span style={{ fontSize: 18, fontWeight: 600 }}>{p.score.toFixed(1)}</span>{" "}
      <span className="muted">
        {p.target === "outcome_5class"
          ? `xRV ${p.scored_quantity >= 0 ? "+" : ""}${p.scored_quantity.toFixed(4)}`
          : `P(whiff|swing) ${(p.scored_quantity * 100).toFixed(1)}%`}
      </span>
    </div>
  );
}

/** ExplanationPanel loads a per-pitch attribution on demand.
 *
 * On demand because attribution costs a model evaluation per feature. A coach
 * asks about one pitch; computing it for every pitch in a stream would put
 * that cost on the live path for something nobody asked to see.
 *
 * The contributions are shown in the model's own margin space, labelled as
 * such. A gradient-boosted model is additive in log-odds and not after the
 * softmax, so presenting these as percentage points of probability would be
 * arithmetic that does not hold -- and it is exactly the kind of plausible
 * wrong number a dashboard makes convincing. */
function ExplanationPanel({ pitchId, variant }: { pitchId: string; variant: string }) {
  const [state, setState] = useState<{
    status: "idle" | "loading" | "done" | "error";
    data?: ExplanationDTO;
    message?: string;
  }>({ status: "idle" });

  if (state.status === "idle") {
    return (
      <button
        onClick={() => {
          setState({ status: "loading" });
          fetch(`/api/v1/pitches/${pitchId}/explanation?variant=${variant}`)
            .then(async (r) => {
              if (!r.ok) {
                const problem = (await r.json().catch(() => null)) as
                  | { detail?: string; title?: string }
                  | null;
                throw new Error(problem?.detail ?? problem?.title ?? `HTTP ${r.status}`);
              }
              return (await r.json()) as ExplanationDTO;
            })
            .then((data) => setState({ status: "done", data }))
            .catch((e: Error) => setState({ status: "error", message: e.message }));
        }}
      >
        Explain this pitch
      </button>
    );
  }

  if (state.status === "loading") return <div className="muted">Computing attribution…</div>;
  if (state.status === "error") {
    return <div className="muted">Explanation unavailable: {state.message}</div>;
  }

  const e = state.data!;
  const magnitude = Math.max(
    ...e.contributions.map((c) => Math.abs(c.contribution)),
    Math.abs(e.other_contribution),
    1e-9,
  );

  return (
    <div>
      <div className="muted" style={{ fontSize: 12, marginBottom: 8 }}>
        Explaining <span className="mono">{e.target_class}</span> in {e.space} space ·{" "}
        base {e.base_value.toFixed(3)} → {e.predicted_value.toFixed(3)} · P ={" "}
        {(e.predicted_probability * 100).toFixed(1)}%
      </div>
      {e.contributions.map((c) => (
        <div className="prob-row" key={c.feature}>
          <div className="name mono" style={{ fontSize: 11 }}>
            {c.feature}
            {c.value !== null && <span className="muted"> {c.value.toFixed(2)}</span>}
          </div>
          <div className="track">
            <div
              className="fill"
              style={{
                width: `${(Math.abs(c.contribution) / magnitude) * 100}%`,
                background: c.contribution >= 0 ? "var(--good)" : "var(--bad)",
              }}
            />
          </div>
          <div className="pct num">{c.contribution >= 0 ? "+" : ""}{c.contribution.toFixed(3)}</div>
        </div>
      ))}
      {/* Everything outside the top features, so the parts add up and the
          list cannot be mistaken for the whole explanation. */}
      <div className="muted" style={{ fontSize: 12, marginTop: 6 }}>
        all other features {e.other_contribution >= 0 ? "+" : ""}
        {e.other_contribution.toFixed(3)}
      </div>
    </div>
  );
}

export function PitchDetail({ pitch }: { pitch: Pitch }) {
  const pitching = pitch.predictions?.find((p) => p.variant === "pitching");
  const stuff = pitch.predictions?.find((p) => p.variant === "stuff");

  return (
    <div>
      <div style={{ marginBottom: 12 }}>
        <div style={{ fontSize: 18, fontWeight: 600 }}>
          #{pitch.session_pitch_index} · {pitch.pitch_name ?? pitch.pitch_type ?? "unknown"}
        </div>
        <div className="muted">
          inning {pitch.context.inning} · {pitch.context.balls}-{pitch.context.strikes} ·{" "}
          {pitch.context.outs_when_up} out · vs {pitch.context.stand}HB
          {pitch.context.on_1b || pitch.context.on_2b || pitch.context.on_3b ? (
            <>
              {" · runners "}
              {[pitch.context.on_1b && "1B", pitch.context.on_2b && "2B", pitch.context.on_3b && "3B"]
                .filter(Boolean)
                .join(" ")}
            </>
          ) : (
            " · bases empty"
          )}
        </div>
      </div>

      {pitch.prediction_status !== "OK" && (
        // A pitch stored without a prediction is not an error to hide. The
        // pipeline keeps the pitch when inference is unavailable, and the
        // dashboard says so rather than rendering a blank score.
        <div className="muted" style={{ marginBottom: 12 }}>
          Prediction unavailable ({pitch.prediction_status.toLowerCase()}). The pitch was stored
          anyway and can be scored later.
        </div>
      )}

      {pitching && <ScoreLine p={pitching} />}
      {stuff && <ScoreLine p={stuff} />}

      {pitching?.probabilities && (
        <div style={{ margin: "12px 0" }}>
          <h2>Predicted vs actual</h2>
          <ProbabilityBars
            probabilities={pitching.probabilities}
            actual={pitch.actual_outcome}
          />
        </div>
      )}

      {pitch.measurement && (
        <div style={{ margin: "12px 0" }}>
          <h2>Measurement</h2>
          <MeasurementTable m={pitch.measurement} />
        </div>
      )}

      <ExplanationPanel pitchId={pitch.id} variant="pitching" />

      {pitch.correlation_id && (
        // The last link in the end-to-end trace. One grep in the server logs
        // for this value returns the whole journey of this pitch.
        <div className="muted mono" style={{ marginTop: 12, fontSize: 11 }}>
          correlation {pitch.correlation_id}
        </div>
      )}
    </div>
  );
}
