import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";

import { api } from "../api/client";
import { Empty, ErrorState, Loading, SyntheticBadge, formatNumber } from "../components/common";

/** Home is a directory, not a dashboard. The three pages the brief describes
 * are the product; this exists so they can be reached without pasting a UUID
 * into the address bar. */
export function Home() {
  const athletes = useQuery({
    queryKey: ["athletes"],
    queryFn: () => api.athletes({ limit: 25, role: "PITCHER" }),
  });

  const live = useQuery({
    queryKey: ["active-sessions"],
    queryFn: () => api.activeSessions({ limit: 10 }),
    // Outings start and finish while the page is open, so this list is worth
    // polling. It is one indexed query and it is what makes the live page
    // reachable during a replay.
    refetchInterval: 10_000,
  });

  return (
    <>
      <div className="panel">
        <h2>Live now</h2>
        {live.isLoading && <Loading what="active outings" />}
        {live.error && <ErrorState error={live.error} what="active outings" />}
        {live.data &&
          (live.data.data.length === 0 ? (
            <Empty>
              Nothing is streaming. Start the replay simulator to put an outing on the wire.
            </Empty>
          ) : (
            <table>
              <thead>
                <tr>
                  <th>Outing</th>
                  <th className="num">Pitches</th>
                  <th className="num">Avg mph</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {live.data.data.map((s) => (
                  <tr key={s.id}>
                    <td>
                      <span className="mono">{s.external_session_uid}</span>{" "}
                      <SyntheticBadge synthetic={s.is_synthetic} />
                    </td>
                    <td className="num">{s.metrics.pitch_count}</td>
                    <td className="num">{formatNumber(s.metrics.avg_release_speed)}</td>
                    <td>
                      <Link to={`/live/${s.id}`}>watch →</Link>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          ))}
      </div>

      <div className="panel">
        <h2>Pitchers</h2>
        {athletes.isLoading && <Loading what="pitchers" />}
        {athletes.error && <ErrorState error={athletes.error} what="pitchers" />}
        {athletes.data &&
          (athletes.data.data.length === 0 ? (
            <Empty>
              No pitchers yet. Load the database and run a replay to populate the dashboard.
            </Empty>
          ) : (
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Throws</th>
                  <th className="num">Outings</th>
                  <th className="num">Pitches</th>
                </tr>
              </thead>
              <tbody>
                {athletes.data.data.map((a) => (
                  <tr key={a.id}>
                    <td>
                      <Link to={`/athletes/${a.id}`}>{a.full_name}</Link>
                    </td>
                    <td>{a.throws ?? "—"}</td>
                    <td className="num">{a.session_count}</td>
                    <td className="num">{a.pitch_count}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          ))}
      </div>
    </>
  );
}
