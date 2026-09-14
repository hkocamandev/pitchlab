import { useQuery } from "@tanstack/react-query";

import { api } from "../api/client";
import { formatDate } from "./common";

/** ModelCard names the models behind every number on the page.
 *
 * A score shown without the model that produced it is not interpretable: the
 * same pitch scores differently under a retrained model, and a dashboard that
 * hides which one it used invites the viewer to compare numbers that are not
 * comparable. */
function MetricSummary({ metrics }: { metrics: unknown }) {
  if (!metrics || typeof metrics !== "object") return null;
  const entries = Object.entries(metrics as Record<string, unknown>).filter(
    ([, v]) => typeof v === "number",
  );
  if (entries.length === 0) return null;

  return (
    <>
      {" · "}
      {entries
        .map(([k, v]) => `${k} ${(v as number).toFixed(3)}`)
        .join(" · ")}
    </>
  );
}

export function ModelCard() {
  const { data } = useQuery({
    queryKey: ["models"],
    queryFn: () => api.models(),
    staleTime: 5 * 60_000,
  });

  const active = (data?.data ?? []).filter((m) => m.is_active);
  if (active.length === 0) return null;

  return (
    <div className="footer">
      {active.map((m) => (
        <div key={`${m.variant}:${m.version}`}>
          <span className="mono">{m.variant}</span> · {m.version} · target{" "}
          <span className="mono">{m.target}</span> · {m.feature_count} features ·
          trained {formatDate(m.train_period.start)} to{" "}
          {formatDate(m.train_period.end)}
          {/* Tested on a window that ends after training ends -- the split is
              temporal, and showing both dates is what makes that checkable. */}
          {" · tested through "}
          {formatDate(m.test_period.end)}
          <MetricSummary metrics={m.metrics} />
        </div>
      ))}
    </div>
  );
}
