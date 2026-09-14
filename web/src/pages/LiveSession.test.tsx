import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { FakeSocket, fakeSocketFactory, livePitch, message } from "../test/fakeSocket";

const SESSION = "00000000-0000-0000-0000-000000000001";

// The API is mocked at the client boundary rather than at fetch. The client is
// the seam the pages actually use, and mocking below it would test the URL
// builder as much as the page.
const sessionPitches = vi.fn();
const session = vi.fn();

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      session: (...args: unknown[]) => session(...args),
      sessionPitches: (...args: unknown[]) => sessionPitches(...args),
      models: () => Promise.resolve({ data: [] }),
    },
  };
});

// The hook is used with an injected socket factory so the page's own effect
// runs unchanged and only the transport is replaced.
vi.mock("../hooks/useSessionStream", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../hooks/useSessionStream")>();
  return {
    ...actual,
    useSessionStream: (id: string | undefined, options: Record<string, unknown> = {}) =>
      actual.useSessionStream(id, {
        ...options,
        socketFactory: fakeSocketFactory,
        random: () => 0,
      }),
  };
});

const { LiveSession } = await import("./LiveSession");

function renderPage(): ReactNode {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 0 } },
  });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[`/live/${SESSION}`]}>
        <Routes>
          <Route path="/live/:sessionId" element={<LiveSession />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return null;
}

beforeEach(() => {
  FakeSocket.reset();
  session.mockReset();
  sessionPitches.mockReset();

  session.mockResolvedValue({
    id: SESSION,
    external_session_uid: "776507:434378",
    pitcher_athlete_id: "a1",
    started_at: "2025-08-31T19:10:00Z",
    status: "ACTIVE",
    is_synthetic: true,
    metrics: { pitch_count: 0, avg_release_speed: null, avg_release_spin_rate: null, avg_pitch_score: null },
    pitcher: { id: "a1", full_name: "Doe, John", throws: "R" },
    anomaly_count: 0,
  });
  sessionPitches.mockResolvedValue({ data: [], page: { limit: 20, has_more: false } });
});

afterEach(() => {
  vi.clearAllMocks();
});

describe("Live session page", () => {
  it("shows a useful empty state before any pitch arrives", async () => {
    renderPage();

    // A blank panel would read as a broken page. Saying what is missing and
    // how to make it appear is the difference between an empty state and a
    // dead one.
    expect(await screen.findByText(/start a replay/i)).toBeInTheDocument();
  });

  it("marks the session as replayed", async () => {
    renderPage();
    expect(await screen.findByText("synthetic")).toBeInTheDocument();
  });

  it("streams pitches into the feed and reports the connection", async () => {
    renderPage();
    await screen.findByText(/start a replay/i);

    act(() => FakeSocket.last.open());
    expect(await screen.findByText("live")).toBeInTheDocument();

    act(() => FakeSocket.last.deliver(message("pitch.analyzed", 1, livePitch(7))));

    // The pitch shows in the feed and, because it is the newest, in the
    // detail panel as well -- so both places are expected to name it.
    await waitFor(() => expect(screen.getAllByText("Whiff").length).toBeGreaterThan(0));
    expect(screen.getAllByText("95.0").length).toBeGreaterThan(0);
    expect(screen.getByText(/Pitch #7/)).toBeInTheDocument();
  });

  it("refetches over REST when the stream skips a sequence number", async () => {
    renderPage();
    await screen.findByText(/start a replay/i);

    act(() => FakeSocket.last.open());
    act(() => FakeSocket.last.deliver(message("pitch.analyzed", 1, livePitch(1))));

    const before = sessionPitches.mock.calls.length;

    // The server dropped 2, 3 and 4 for this connection. They are not coming
    // back over the socket, so the page has to go and get them.
    act(() => FakeSocket.last.deliver(message("pitch.analyzed", 5, livePitch(5))));

    await waitFor(() =>
      expect(sessionPitches.mock.calls.length).toBeGreaterThan(before),
    );
    expect(await screen.findByText(/3 messages were dropped/i)).toBeInTheDocument();
  });

  it("reports an anomaly when one arrives", async () => {
    renderPage();
    await screen.findByText(/start a replay/i);

    act(() => FakeSocket.last.open());
    act(() =>
      FakeSocket.last.deliver(
        message("anomaly.detected", 1, {
          anomaly_id: "an1",
          metric: "release_speed",
          pitch_type: "FF",
          observed_value: 92.4,
          baseline_median: 94.2,
          z_robust: -3.4,
          direction: "DECREASE",
          severity: "HIGH",
        }),
      ),
    );

    expect(await screen.findByText(/anomaly detected/i)).toBeInTheDocument();
    expect(screen.getByText("HIGH")).toBeInTheDocument();
  });

  it("says the outing is over rather than reconnecting forever", async () => {
    renderPage();
    await screen.findByText(/start a replay/i);

    act(() => FakeSocket.last.open());
    act(() => FakeSocket.last.deliver(message("session.closed", 1, { reason: "COMPLETED" })));

    expect(await screen.findByText(/outing ended/i)).toBeInTheDocument();
  });
});
