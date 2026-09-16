import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { App } from "../App";
import { ApiError, type Problem } from "../api/client";
import { AuthProvider } from "./AuthContext";

// Mocked at the client boundary, like the other page tests: the client is the
// seam the app uses, and mocking fetch instead would test URL building.
const me = vi.fn();
const login = vi.fn();
const logout = vi.fn();

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      me: (...args: unknown[]) => me(...args),
      login: (...args: unknown[]) => login(...args),
      logout: (...args: unknown[]) => logout(...args),
      athletes: () => Promise.resolve({ data: [], page: {} }),
      activeSessions: () => Promise.resolve({ data: [], page: {} }),
    },
  };
});

const USER = {
  id: "00000000-0000-0000-0000-0000000000aa",
  username: "ada",
  display_name: "Ada Lovelace",
  role: "viewer",
};

function problem(status: number, detail: string): ApiError {
  const body: Problem = { type: "", title: "", status, detail };
  return new ApiError(status, body, detail);
}

function renderApp() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/"]}>
        <AuthProvider>
          <App />
        </AuthProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("the session gate", () => {
  beforeEach(() => {
    me.mockReset();
    login.mockReset();
    logout.mockReset();
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("shows the login form when nobody is signed in", async () => {
    me.mockRejectedValue(problem(401, "no session"));

    renderApp();

    expect(await screen.findByRole("button", { name: /sign in/i })).toBeTruthy();
    // The application itself must not be behind it. Rendering the dashboard
    // and covering it with a form would ship the data to the browser anyway.
    expect(screen.queryByRole("navigation")).toBeNull();
  });

  it("does not flash the login form while the session is still unknown", async () => {
    // Never resolves: this is the state between load and the first answer.
    me.mockReturnValue(new Promise(() => {}));

    renderApp();

    await waitFor(() => {
      expect(screen.queryByRole("button", { name: /sign in/i })).toBeNull();
    });
  });

  it("shows the application to a signed-in user", async () => {
    me.mockResolvedValue({
      user: USER,
      expires_at: new Date(Date.now() + 86_400_000).toISOString(),
    });

    renderApp();

    expect(await screen.findByText("Ada Lovelace")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^sign in$/i })).toBeNull();
  });

  it("shows the role, because it changes what the API will allow", async () => {
    me.mockResolvedValue({
      user: USER,
      expires_at: new Date(Date.now() + 86_400_000).toISOString(),
    });

    renderApp();

    expect(await screen.findByText("viewer")).toBeTruthy();
  });
});

describe("signing in", () => {
  beforeEach(() => {
    me.mockReset();
    login.mockReset();
    logout.mockReset();
    me.mockRejectedValue(problem(401, "no session"));
  });

  it("swaps the form for the application on success", async () => {
    const user = userEvent.setup();
    login.mockResolvedValue({
      user: USER,
      expires_at: new Date(Date.now() + 86_400_000).toISOString(),
    });

    renderApp();
    await screen.findByRole("button", { name: /sign in/i });

    await user.type(screen.getByLabelText(/username/i), "ada");
    await user.type(screen.getByLabelText(/password/i), "correct horse");
    await user.click(screen.getByRole("button", { name: /sign in/i }));

    expect(await screen.findByText("Ada Lovelace")).toBeTruthy();
    expect(login).toHaveBeenCalledWith("ada", "correct horse");
  });

  it("gives one message for a wrong password, not two", async () => {
    const user = userEvent.setup();
    // The API answers the same 401 whether the user exists or not, and the
    // form must not invent a distinction the server refused to make.
    login.mockRejectedValue(problem(401, "invalid username or password"));

    renderApp();
    await screen.findByRole("button", { name: /sign in/i });

    await user.type(screen.getByLabelText(/username/i), "ada");
    await user.type(screen.getByLabelText(/password/i), "wrong");
    await user.click(screen.getByRole("button", { name: /sign in/i }));

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toMatch(/did not match/i);
    // It must not say *which* half was wrong. "No such user" enumerates
    // accounts; "wrong password" confirms one exists.
    expect(alert.textContent).not.toMatch(
      /no such|does not exist|not found|unknown user|wrong password|incorrect password/i,
    );
  });

  it("says plainly when the account is throttled", async () => {
    const user = userEvent.setup();
    // The one failure worth distinguishing: retrying makes it worse, and
    // somebody who is not told will keep retrying.
    login.mockRejectedValue(
      problem(429, "too many login attempts; try again in a few minutes"),
    );

    renderApp();
    await screen.findByRole("button", { name: /sign in/i });

    await user.type(screen.getByLabelText(/username/i), "ada");
    await user.type(screen.getByLabelText(/password/i), "wrong");
    await user.click(screen.getByRole("button", { name: /sign in/i }));

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toMatch(/too many|few minutes/i);
  });

  it("clears the password after a failure", async () => {
    const user = userEvent.setup();
    login.mockRejectedValue(problem(401, "invalid username or password"));

    renderApp();
    await screen.findByRole("button", { name: /sign in/i });

    await user.type(screen.getByLabelText(/username/i), "ada");
    await user.type(screen.getByLabelText(/password/i), "wrong");
    await user.click(screen.getByRole("button", { name: /sign in/i }));
    await screen.findByRole("alert");

    // A wrong password left in the field is a wrong password resubmitted by
    // the next press of enter -- which spends throttle attempts.
    expect((screen.getByLabelText(/password/i) as HTMLInputElement).value).toBe("");
    // The username stays: retyping it is pure friction.
    expect((screen.getByLabelText(/username/i) as HTMLInputElement).value).toBe("ada");
  });

  it("never renders the password in the page as text", async () => {
    const user = userEvent.setup();
    renderApp();
    await screen.findByRole("button", { name: /sign in/i });

    await user.type(screen.getByLabelText(/password/i), "hunter2");

    const field = screen.getByLabelText(/password/i) as HTMLInputElement;
    expect(field.type).toBe("password");
    expect(document.body.textContent).not.toContain("hunter2");
  });
});

describe("signing out", () => {
  beforeEach(() => {
    me.mockReset();
    login.mockReset();
    logout.mockReset();
  });

  it("returns to the login form", async () => {
    const user = userEvent.setup();
    me.mockResolvedValue({
      user: USER,
      expires_at: new Date(Date.now() + 86_400_000).toISOString(),
    });
    logout.mockResolvedValue({ status: "signed out" });

    renderApp();
    await screen.findByText("Ada Lovelace");

    await user.click(screen.getByRole("button", { name: /sign out/i }));

    expect(await screen.findByRole("button", { name: /^sign in$/i })).toBeTruthy();
  });

  it("signs out locally even when the request fails", async () => {
    const user = userEvent.setup();
    me.mockResolvedValue({
      user: USER,
      expires_at: new Date(Date.now() + 86_400_000).toISOString(),
    });
    logout.mockRejectedValue(new Error("network down"));

    renderApp();
    await screen.findByText("Ada Lovelace");

    await user.click(screen.getByRole("button", { name: /sign out/i }));

    // Staying signed in because signing out failed is the worse of the two
    // lies: the person asked to leave and the screen says they did not.
    expect(await screen.findByRole("button", { name: /^sign in$/i })).toBeTruthy();
  });
});
