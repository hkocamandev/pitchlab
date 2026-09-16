import { useState, type FormEvent } from "react";

import { ApiError } from "../api/client";
import { useAuth } from "../auth/AuthContext";

/** Login is the only page reachable without a session.
 *
 * It shows one message for every kind of failure, because the API returns one
 * message for every kind of failure: telling a stranger that the username was
 * right and only the password was wrong hands them half the credential. The
 * exception is the throttle, which has to be distinguishable -- somebody
 * locked out needs to know that waiting is the answer and retrying is not. */
export function Login() {
  const { login } = useAuth();

  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (busy) return;

    setBusy(true);
    setError(null);
    try {
      await login(username.trim(), password);
    } catch (err) {
      if (err instanceof ApiError && err.status === 429) {
        setError(
          err.problem?.detail ??
            "Too many attempts. Wait a few minutes before trying again.",
        );
      } else if (err instanceof ApiError && err.status === 503) {
        // Worth separating: it means the server has no signing key, and no
        // amount of retyping a password will fix it.
        setError("Sign-in is not configured on this server.");
      } else {
        setError("That username and password did not match.");
      }
      // Cleared on failure, kept on success -- a wrong password left in the
      // field is a wrong password submitted again by someone hitting enter.
      setPassword("");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="login">
      <form className="panel login-card" onSubmit={submit}>
        <h2>Sign in</h2>
        <p className="muted">
          PitchLab is private. Sign in to see pitcher analytics and live
          outings.
        </p>

        <label htmlFor="username">Username</label>
        <input
          id="username"
          name="username"
          type="text"
          autoComplete="username"
          autoFocus
          required
          value={username}
          onChange={(e) => setUsername(e.target.value)}
        />

        <label htmlFor="password">Password</label>
        <input
          id="password"
          name="password"
          type="password"
          autoComplete="current-password"
          required
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />

        {/* role="alert" so a screen reader announces the failure rather than
            leaving somebody wondering why nothing happened. */}
        {error && (
          <p className="login-error" role="alert">
            {error}
          </p>
        )}

        <button type="submit" disabled={busy || !username || !password}>
          {busy ? "Signing in…" : "Sign in"}
        </button>
      </form>
    </div>
  );
}
