import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { FakeSocket, fakeSocketFactory, livePitch, message } from "../test/fakeSocket";
import { backoffFor, initialState, reduce, useSessionStream } from "./useSessionStream";

const SESSION = "00000000-0000-0000-0000-000000000001";

// A deterministic "random" so the jitter does not make the backoff assertions
// flaky. The jitter is still exercised -- it is added, just predictably.
const noJitter = () => 0;

beforeEach(() => {
  FakeSocket.reset();
  vi.useFakeTimers({ shouldAdvanceTime: true });
});

afterEach(() => {
  vi.useRealTimers();
});

function renderStream(options: Record<string, unknown> = {}) {
  return renderHook(() =>
    useSessionStream(SESSION, {
      socketFactory: fakeSocketFactory,
      random: noJitter,
      ...options,
    }),
  );
}

// --- T9.2 connection lifecycle --------------------------------------------

describe("connection lifecycle", () => {
  it("connects, receives a snapshot, then streams pitches", async () => {
    const { result } = renderStream();

    expect(result.current.status).toBe("connecting");
    expect(FakeSocket.instances).toHaveLength(1);
    expect(FakeSocket.last.url).toContain(`/ws/sessions/${SESSION}`);

    act(() => FakeSocket.last.open());
    expect(result.current.status).toBe("live");

    act(() =>
      FakeSocket.last.deliver(
        message("session.snapshot", 0, {
          metrics: { pitch_count: 62, avg_release_speed: 93.4, avg_pitch_score: 101.7 },
          last_pitch_index: 62,
        }),
      ),
    );

    expect(result.current.snapshot?.pitchCount).toBe(62);
    expect(result.current.snapshot?.lastPitchIndex).toBe(62);

    act(() => FakeSocket.last.deliver(message("pitch.analyzed", 1, livePitch(63))));

    expect(result.current.pitches).toHaveLength(1);
    expect(result.current.pitches[0]?.session_pitch_index).toBe(63);
    // No gap: the snapshot was 0 and the pitch is 1.
    expect(result.current.missed).toBe(0);
  });

  it("closes the socket when the component unmounts", () => {
    const { unmount } = renderStream();
    act(() => FakeSocket.last.open());
    const socket = FakeSocket.last;

    unmount();

    // Left open, the connection would keep a slot on the server and keep
    // dispatching into a component that no longer exists.
    expect(socket.closedWith?.code).toBe(1000);
  });

  it("does not reconnect after unmounting", async () => {
    const { unmount } = renderStream();
    act(() => FakeSocket.last.open());
    const socket = FakeSocket.last;

    unmount();
    act(() => socket.serverClose(1006));

    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });

    // A close event queued by the browser must not schedule a reconnect for a
    // page the user has left, which is how a single-page app ends up holding
    // sockets for every session it has ever shown.
    expect(FakeSocket.instances).toHaveLength(1);
  });

  it("ignores frames that are not valid JSON", () => {
    const { result } = renderStream();
    act(() => FakeSocket.last.open());

    act(() => FakeSocket.last.deliverRaw("{not json"));

    // One malformed frame should not end a session that is otherwise fine.
    expect(result.current.status).toBe("live");
  });
});

// --- T9.4 reconnection ----------------------------------------------------

describe("reconnection", () => {
  it("retries with exponential backoff after an unexpected close", async () => {
    const { result } = renderStream();
    act(() => FakeSocket.last.open());

    act(() => FakeSocket.last.serverClose(1006));
    expect(result.current.status).toBe("reconnecting");

    // Nothing yet: the first retry waits a second.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(500);
    });
    expect(FakeSocket.instances).toHaveLength(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(600);
    });
    expect(FakeSocket.instances).toHaveLength(2);

    // Second failure waits twice as long. Backing off matters because a
    // server that just restarted must not be met by every open dashboard at
    // once.
    act(() => FakeSocket.last.serverClose(1006));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1500);
    });
    expect(FakeSocket.instances).toHaveLength(2);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(700);
    });
    expect(FakeSocket.instances).toHaveLength(3);
  });

  it("does not retry after a normal close", async () => {
    const { result } = renderStream();
    act(() => FakeSocket.last.open());

    // 1000 means the outing is over. Retrying would reconnect forever to a
    // session that will never produce another pitch.
    act(() => FakeSocket.last.serverClose(1000));

    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });

    expect(FakeSocket.instances).toHaveLength(1);
    expect(result.current.status).toBe("closed");
  });

  it("resets the sequence baseline on reconnect", async () => {
    const onGap = vi.fn();
    const { result } = renderStream({ onGap });

    act(() => FakeSocket.last.open());
    act(() => FakeSocket.last.deliver(message("pitch.analyzed", 40, livePitch(40))));
    act(() => FakeSocket.last.deliver(message("pitch.analyzed", 41, livePitch(41))));
    expect(result.current.lastSeq).toBe(41);

    act(() => FakeSocket.last.serverClose(1006));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1100);
    });
    act(() => FakeSocket.last.open());

    onGap.mockClear();
    // Sequence numbers are per connection, so the new connection starts at 0
    // again. Not resetting would read the fresh start as a 41-message gap.
    act(() => FakeSocket.last.deliver(message("session.snapshot", 0, { last_pitch_index: 41 })));

    expect(onGap).not.toHaveBeenCalled();
  });

  it("tells the page to reconcile after reconnecting", async () => {
    const onReconnect = vi.fn();
    renderStream({ onReconnect });

    act(() => FakeSocket.last.open());
    expect(onReconnect).not.toHaveBeenCalled(); // not on the first connection

    act(() => FakeSocket.last.serverClose(1006));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1100);
    });
    act(() => FakeSocket.last.open());

    // Pitches thrown while the socket was down were never delivered, so the
    // page has to re-read rather than assume it is current.
    await waitFor(() => expect(onReconnect).toHaveBeenCalledTimes(1));
  });
});

// --- T9.3 sequence gaps ---------------------------------------------------

describe("sequence gaps", () => {
  it("detects a gap and reports how many messages were missed", async () => {
    const onGap = vi.fn();
    const { result } = renderStream({ onGap });

    act(() => FakeSocket.last.open());
    act(() => FakeSocket.last.deliver(message("pitch.analyzed", 1, livePitch(1))));
    act(() => FakeSocket.last.deliver(message("pitch.analyzed", 2, livePitch(2))));
    expect(result.current.missed).toBe(0);

    // The server dropped 3, 4 and 5 because this client fell behind. They are
    // gone -- the gap is the only evidence, which is exactly what the
    // sequence number is for.
    act(() => FakeSocket.last.deliver(message("pitch.analyzed", 6, livePitch(6))));

    expect(result.current.missed).toBe(3);
    await waitFor(() => expect(onGap).toHaveBeenCalledWith(3));
  });

  it("does not report a gap for the first message on a connection", async () => {
    const onGap = vi.fn();
    const { result } = renderStream({ onGap });

    act(() => FakeSocket.last.open());
    act(() => FakeSocket.last.deliver(message("session.snapshot", 0, { last_pitch_index: 0 })));

    expect(result.current.missed).toBe(0);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10);
    });
    expect(onGap).not.toHaveBeenCalled();
  });
});

// --- payload handling -----------------------------------------------------

describe("payload handling", () => {
  it("ignores a pitch it has already shown", () => {
    const { result } = renderStream();
    act(() => FakeSocket.last.open());

    const pitch = livePitch(7);
    act(() => FakeSocket.last.deliver(message("pitch.analyzed", 1, pitch)));
    act(() => FakeSocket.last.deliver(message("pitch.analyzed", 2, pitch)));

    // At-least-once delivery reaches the browser too: a reconnect can replay
    // a pitch the feed already shows.
    expect(result.current.pitches).toHaveLength(1);
  });

  it("surfaces anomalies and the terminal message", () => {
    const { result } = renderStream();
    act(() => FakeSocket.last.open());

    act(() =>
      FakeSocket.last.deliver(
        message("anomaly.detected", 1, {
          anomaly_id: "a1",
          metric: "release_speed",
          z_robust: -3.4,
          severity: "HIGH",
        }),
      ),
    );
    expect(result.current.anomalies).toHaveLength(1);

    act(() => FakeSocket.last.deliver(message("session.closed", 2, { reason: "COMPLETED" })));
    expect(result.current.status).toBe("closed");
    expect(result.current.closedReason).toBe("COMPLETED");
  });

  it("surfaces a slow-consumer warning", () => {
    const { result } = renderStream();
    act(() => FakeSocket.last.open());

    act(() =>
      FakeSocket.last.deliver(
        message("error", 1, { code: "SLOW_CONSUMER", message: "client is not keeping up" }),
      ),
    );

    expect(result.current.error).toContain("keeping up");
  });
});

// --- pure pieces ----------------------------------------------------------

describe("reducer", () => {
  it("is pure", () => {
    const before = structuredClone(initialState);
    reduce(initialState, { type: "message", message: message("pitch.analyzed", 1, livePitch(1)) });
    expect(initialState).toEqual(before);
  });

  it("caps the feed", () => {
    let state = reduce(initialState, { type: "open" });
    for (let i = 0; i < 100; i++) {
      state = reduce(state, {
        type: "message",
        message: message("pitch.analyzed", i, livePitch(i)),
      });
    }
    // An unbounded list in a component that re-renders per pitch gets slower
    // the longer you watch.
    expect(state.pitches.length).toBeLessThanOrEqual(40);
    expect(state.pitches[0]?.session_pitch_index).toBe(99);
  });
});

describe("backoff", () => {
  it("grows and then caps", () => {
    const delays = [0, 1, 2, 3, 4, 5, 6, 10].map((n) => backoffFor(n, noJitter));
    expect(delays).toEqual([1000, 2000, 4000, 8000, 16000, 30000, 30000, 30000]);
  });

  it("adds jitter", () => {
    // Without jitter every reconnecting client returns at the same instant,
    // which is the thundering herd a backoff is supposed to prevent.
    expect(backoffFor(0, () => 0.5)).toBe(1500);
  });
});
