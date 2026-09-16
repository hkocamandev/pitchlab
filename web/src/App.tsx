import { Link, Route, Routes } from "react-router-dom";

import { useAuth } from "./auth/AuthContext";
import { AthleteOverview } from "./pages/AthleteOverview";
import { Home } from "./pages/Home";
import { LiveSession } from "./pages/LiveSession";
import { Login } from "./pages/Login";
import { SessionDetail } from "./pages/SessionDetail";

export function App() {
  const { status } = useAuth();

  // Nothing is rendered until the session is known. This is one render frame
  // in practice and it is what keeps a reload from flashing the login form at
  // somebody who is already signed in.
  if (status === "checking") {
    return <div className="state">Loading…</div>;
  }

  // One gate for the whole application rather than a guard on each route.
  // A route added later is protected because it is inside this branch, not
  // because somebody remembered to wrap it -- and forgetting to wrap a route
  // is the single most common way a dashboard leaks.
  if (status === "anonymous") {
    return <Login />;
  }

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
        <SessionBadge />
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

/** SessionBadge shows who is signed in and offers the way out.
 *
 * The role is shown because it changes what the person can do: a viewer's
 * write buttons are refused by the API, and seeing "viewer" is the difference
 * between understanding that and filing a bug. */
function SessionBadge() {
  const { user, logout } = useAuth();
  if (!user) return null;

  return (
    <div className="session-badge">
      <span className="muted" style={{ fontSize: 12 }}>
        {user.display_name || user.username}
        <span className="role-chip">{user.role}</span>
      </span>
      <button type="button" className="linkish" onClick={() => void logout()}>
        sign out
      </button>
    </div>
  );
}
