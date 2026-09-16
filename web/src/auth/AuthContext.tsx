import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";

import { ApiError, api, onUnauthorized, type User } from "../api/client";

// The session state has three values, not two.
//
// "checking" is the one that is easy to leave out and impossible to do
// without: on load the app cannot tell whether it is signed in, because the
// cookie is httpOnly and there is nothing to read. It has to ask the API, and
// until the answer arrives it must render neither the dashboard nor the login
// form -- rendering the form first makes every reload flash a login screen at
// somebody who is already signed in.
type Status = "checking" | "authenticated" | "anonymous";

interface AuthState {
  status: Status;
  user: User | null;
  expiresAt: Date | null;
  login: (username: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
}

const AuthContext = createContext<AuthState | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<Status>("checking");
  const [user, setUser] = useState<User | null>(null);
  const [expiresAt, setExpiresAt] = useState<Date | null>(null);

  const clear = useCallback(() => {
    setUser(null);
    setExpiresAt(null);
    setStatus("anonymous");
  }, []);

  // Any request that comes back 401 drops the app to the login screen, from
  // wherever it was. The token lasts a day; a dashboard left open overnight
  // will meet this.
  useEffect(() => {
    onUnauthorized(clear);
    return () => onUnauthorized(null);
  }, [clear]);

  useEffect(() => {
    const controller = new AbortController();

    api
      .me(controller.signal)
      .then((session) => {
        setUser(session.user);
        setExpiresAt(new Date(session.expires_at));
        setStatus("authenticated");
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        // A 401 here is the ordinary case -- nobody is signed in. Anything
        // else is a broken API, and the honest thing is still to show the
        // login page rather than a dashboard that cannot load.
        if (!(err instanceof ApiError) || err.status !== 401) {
          console.warn("could not establish the session", err);
        }
        clear();
      });

    return () => controller.abort();
  }, [clear]);

  const login = useCallback(async (username: string, password: string) => {
    const session = await api.login(username, password);
    setUser(session.user);
    setExpiresAt(new Date(session.expires_at));
    setStatus("authenticated");
  }, []);

  const logout = useCallback(async () => {
    try {
      await api.logout();
    } catch (err) {
      // Swallowed on purpose, and this is the whole reason the function does
      // not reject: there is nothing a caller could usefully do about a failed
      // sign-out, and a rejection nobody handles surfaces as an unhandled
      // promise error in the console instead.
      console.warn("sign-out request failed; clearing the session anyway", err);
    } finally {
      // Cleared even if the request failed. The cookie may well survive, but a
      // UI that says you are still signed in because signing out went wrong is
      // the worse of the two lies.
      clear();
    }
  }, [clear]);

  const value = useMemo(
    () => ({ status, user, expiresAt, login, logout }),
    [status, user, expiresAt, login, logout],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext);
  if (!ctx) {
    throw new Error("useAuth was called outside an AuthProvider");
  }
  return ctx;
}
