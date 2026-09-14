import { Link, Route, Routes } from "react-router-dom";

import { AthleteOverview } from "./pages/AthleteOverview";
import { Home } from "./pages/Home";
import { LiveSession } from "./pages/LiveSession";
import { SessionDetail } from "./pages/SessionDetail";

export function App() {
  return (
    <div className="app">
      <header className="topbar">
        <h1>
          <Link to="/" style={{ color: "inherit" }}>
            PitchLab
          </Link>
        </h1>
        <nav>
          <Link to="/">Pitchers</Link>
        </nav>
        <div className="spacer" />
        <span className="muted" style={{ fontSize: 12 }}>
          pitch analytics over a live event pipeline
        </span>
      </header>

      <Routes>
        <Route path="/" element={<Home />} />
        <Route path="/athletes/:athleteId" element={<AthleteOverview />} />
        <Route path="/sessions/:sessionId" element={<SessionDetail />} />
        <Route path="/live/:sessionId" element={<LiveSession />} />
        <Route
          path="*"
          element={
            <div className="state">
              Nothing here. <Link to="/">Back to pitchers.</Link>
            </div>
          }
        />
      </Routes>
    </div>
  );
}
