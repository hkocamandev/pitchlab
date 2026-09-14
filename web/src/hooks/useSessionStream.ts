import { useCallback, useEffect, useReducer, useRef } from "react";

import type {
  LiveAnomaly,
  LiveMetrics,
  LivePitch,
  WSClosed,
  WSError,
  WSMessage,
} from "../api/types.gen";

/** How many pitches the live feed keeps. The rest are on the timeline page;
 * holding an unbounded list in a component that re-renders per pitch is how a
 * live view gets slower the longer you watch it. */
export const LIVE_PITCH_LIMIT = 40;

export type ConnectionStatus = "connecting" | "live" | "reconnecting" | "closed";

export interface SnapshotSummary {
  pitchCount: number;
  avgReleaseSpeed: number | null;
  avgPitchScore: number | null;
  lastPitchIndex: number;
}

export interface StreamState {
  status: ConnectionStatus;
  /** Newest first. */
  pitches: LivePitch[];
  metrics: LiveMetrics | null;
  anomalies: LiveAnomaly[];
  snapshot: SnapshotSummary | null;
  /** Sequence number of the last message received on this connection. */
  lastSeq: number;
  /** How many messages the server dropped for this connection. A non-zero
   * value is the signal to reconcile over REST -- it is why the sequence
   * number exists. */
  missed: number;
  /** Reconnection attempts since the last successful open. */
  attempt: number;
  error: string | null;
  closedReason: string | null;
}

export const initialState: StreamState = {
  status: "connecting",
  pitches: [],
  metrics: null,
  anomalies: [],
  snapshot: null,
  lastSeq: -1,
  missed: 0,
  attempt: 0,
  error: null,
  closedReason: null,
};

type Action =
  | { type: "connecting"; attempt: number }
  | { type: "open" }
  | { type: "message"; message: WSMessage }
  | { type: "socket-error" }
  | { type: "disconnected"; willRetry: boolean };

/** reduce is a pure function of the previous state and one event, which is the
 * reason a reducer is used here rather than a handful of useState calls: a
 * pitch arriving has to update the feed, the sequence number, the miss counter
 * and possibly the connection status together, and four independent setters
 * would let a render observe some of that and not the rest. */
export function reduce(state: StreamState, action: Action): StreamState {
  switch (action.type) {
    case "connecting":
      return {
        ...state,
        status: action.attempt === 0 ? "connecting" : "reconnecting",
        attempt: action.attempt,
        error: null,
      };

    case "open":
      // Sequence numbers are per connection, so a reconnect starts over. Not
      // resetting would make the first message of the new connection look like
      // a gap of however far the old one had counted.
      return { ...state, status: "live", attempt: 0, lastSeq: -1, error: null };

    case "socket-error":
      return { ...state, error: "connection failed" };

    case "disconnected":
      return {
        ...state,
        status: action.willRetry ? "reconnecting" : "closed",
      };

    case "message":
      return applyMessage(state, action.message);
  }
}

function applyMessage(state: StreamState, message: WSMessage): StreamState {
  // A gap means the server dropped messages for this connection because it
  // could not keep up. The count is reported rather than hidden: the page uses
  // it to refetch over REST.
  const expected = state.lastSeq + 1;
  const missed =
    state.lastSeq >= 0 && message.seq > expected
      ? state.missed + (message.seq - expected)
      : state.missed;

  const next: StreamState = { ...state, lastSeq: message.seq, missed };

  switch (message.type) {
    case "session.snapshot": {
      const data = message.data as {
        metrics?: {
          pitch_count?: number;
          avg_release_speed?: number | null;
          avg_pitch_score?: number | null;
        };
        last_pitch_index?: number;
      };
      return {
        ...next,
        snapshot: {
          pitchCount: data.metrics?.pitch_count ?? 0,
          avgReleaseSpeed: data.metrics?.avg_release_speed ?? null,
          avgPitchScore: data.metrics?.avg_pitch_score ?? null,
          lastPitchIndex: data.last_pitch_index ?? 0,
        },
      };
    }

    case "pitch.analyzed": {
      const pitch = message.data as LivePitch;
      // Guarded because at-least-once delivery reaches the browser too: a
      // reconnect can replay a pitch the feed already shows.
      if (next.pitches.some((p) => p.pitch_id === pitch.pitch_id)) {
        return next;
      }
      return {
        ...next,
        pitches: [pitch, ...next.pitches].slice(0, LIVE_PITCH_LIMIT),
      };
    }

    case "session.metrics":
      return { ...next, metrics: message.data as LiveMetrics };

    case "anomaly.detected":
      return {
        ...next,
        anomalies: [message.data as LiveAnomaly, ...next.anomalies].slice(0, 10),
      };

    case "session.closed":
      return {
        ...next,
        status: "closed",
        closedReason: (message.data as WSClosed).reason,
      };

    case "error":
      return { ...next, error: (message.data as WSError).message };

    default:
      return next;
  }
}

/** Backoff schedule, in milliseconds. Capped, and jittered at use: a server
 * that has just restarted must not be met by every open dashboard at the same
 * instant. */
const BACKOFF_MS = [1000, 2000, 4000, 8000, 16000, 30000];

export function backoffFor(attempt: number, random: () => number = Math.random): number {
  const base = BACKOFF_MS[Math.min(attempt, BACKOFF_MS.length - 1)] ?? 30000;
  return base + Math.floor(random() * 1000);
}

export interface StreamOptions {
  /** Called when the sequence number jumps, so the page can reconcile the
   * data it missed over REST. */
  onGap?: (missed: number) => void;
  /** Called after every successful (re)connection, so the page can refresh a
   * snapshot it may have drifted from while disconnected. */
  onReconnect?: () => void;
  /** Injected in tests. */
  socketFactory?: (url: string) => WebSocket;
  random?: () => number;
  enabled?: boolean;
}

/** useSessionStream subscribes to one outing's live channel.
 *
 * The connection is owned by an effect keyed on the session, and every timer
 * and socket it creates is torn down by that effect's cleanup. Reconnection is
 * the client's responsibility -- the server does not retry on its behalf. */
export function useSessionStream(sessionId: string | undefined, options: StreamOptions = {}) {
  const [state, dispatch] = useReducer(reduce, initialState);

  const { onGap, onReconnect, socketFactory, random, enabled = true } = options;

  // Every option is held in a ref, and the connection effect depends only on
  // the session and whether it is enabled.
  //
  // This is not tidiness. A caller that passes an inline callback -- which is
  // the natural way to write one -- hands this hook a new function identity on
  // every render. If those identities were effect dependencies, each render
  // would tear the socket down and open another one, and a page that
  // re-renders per pitch would reconnect per pitch. A page-level test found
  // exactly that, as a loop that exhausted memory.
  const gapRef = useRef(onGap);
  const reconnectRef = useRef(onReconnect);
  const factoryRef = useRef(socketFactory);
  const randomRef = useRef(random);
  gapRef.current = onGap;
  reconnectRef.current = onReconnect;
  factoryRef.current = socketFactory;
  randomRef.current = random;

  const missedRef = useRef(0);

  useEffect(() => {
    if (!sessionId || !enabled) return;

    let disposed = false;
    let socket: WebSocket | null = null;
    let timer: ReturnType<typeof setTimeout> | null = null;
    let attempt = 0;

    const url = `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/ws/sessions/${sessionId}`;

    const connect = () => {
      if (disposed) return;

      dispatch({ type: "connecting", attempt });
      const factory = factoryRef.current;
      socket = factory ? factory(url) : new WebSocket(url);

      socket.onopen = () => {
        if (disposed) return;
        const reconnected = attempt > 0;
        attempt = 0;
        dispatch({ type: "open" });
        if (reconnected) reconnectRef.current?.();
      };

      socket.onmessage = (event: MessageEvent) => {
        if (disposed) return;
        let message: WSMessage;
        try {
          message = JSON.parse(event.data as string) as WSMessage;
        } catch {
          // A frame that does not parse is dropped rather than fatal: one bad
          // message should not end a session that is otherwise streaming.
          return;
        }
        dispatch({ type: "message", message });
      };

      socket.onerror = () => {
        if (!disposed) dispatch({ type: "socket-error" });
      };

      socket.onclose = (event: CloseEvent) => {
        if (disposed) return;

        // 1000 means the outing is over: there is nothing to come back for,
        // and retrying would reconnect forever to a finished session.
        const willRetry = event.code !== 1000;
        dispatch({ type: "disconnected", willRetry });
        if (!willRetry) return;

        const delay = backoffFor(attempt, randomRef.current);
        attempt += 1;
        timer = setTimeout(connect, delay);
      };
    };

    connect();

    return () => {
      disposed = true;
      if (timer) clearTimeout(timer);
      if (socket) {
        // Detached before closing so a close event queued by the browser
        // cannot schedule a reconnect for a session the page has left.
        socket.onopen = null;
        socket.onmessage = null;
        socket.onerror = null;
        socket.onclose = null;
        if (socket.readyState === WebSocket.OPEN || socket.readyState === WebSocket.CONNECTING) {
          socket.close(1000, "navigated away");
        }
      }
    };
  }, [sessionId, enabled]);

  // The gap callback fires from an effect rather than from the reducer, which
  // has to stay pure.
  useEffect(() => {
    if (state.missed > missedRef.current) {
      const delta = state.missed - missedRef.current;
      missedRef.current = state.missed;
      gapRef.current?.(delta);
    }
  }, [state.missed]);

  const reset = useCallback(() => {
    missedRef.current = 0;
  }, []);

  return { ...state, resetMissed: reset };
}
