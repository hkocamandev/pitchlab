/** FakeSocket is a WebSocket the tests drive by hand.
 *
 * Written rather than mocked with a library so the tests can be explicit about
 * the three things that actually matter here: when the socket opens, what it
 * delivers, and how it closes. A close code of 1000 means something different
 * from 1006, and the hook has to behave differently for each. */
export class FakeSocket {
  static instances: FakeSocket[] = [];

  static reset(): void {
    FakeSocket.instances = [];
  }

  static get last(): FakeSocket {
    const s = FakeSocket.instances[FakeSocket.instances.length - 1];
    if (!s) throw new Error("no socket was created");
    return s;
  }

  readonly url: string;
  readyState = 0; // CONNECTING
  closedWith: { code: number; reason: string } | null = null;

  onopen: ((this: WebSocket, ev: Event) => unknown) | null = null;
  onmessage: ((this: WebSocket, ev: MessageEvent) => unknown) | null = null;
  onerror: ((this: WebSocket, ev: Event) => unknown) | null = null;
  onclose: ((this: WebSocket, ev: CloseEvent) => unknown) | null = null;

  constructor(url: string) {
    this.url = url;
    FakeSocket.instances.push(this);
  }

  open(): void {
    this.readyState = 1; // OPEN
    this.onopen?.call(this as unknown as WebSocket, new Event("open"));
  }

  deliver(message: unknown): void {
    this.onmessage?.call(
      this as unknown as WebSocket,
      new MessageEvent("message", { data: JSON.stringify(message) }),
    );
  }

  /** deliverRaw sends a frame that is not valid JSON. */
  deliverRaw(data: string): void {
    this.onmessage?.call(
      this as unknown as WebSocket,
      new MessageEvent("message", { data }),
    );
  }

  fail(): void {
    this.onerror?.call(this as unknown as WebSocket, new Event("error"));
  }

  /** serverClose simulates the connection dropping from the other end. */
  serverClose(code = 1006, reason = ""): void {
    this.readyState = 3; // CLOSED
    this.onclose?.call(
      this as unknown as WebSocket,
      new CloseEvent("close", { code, reason }),
    );
  }

  close(code = 1000, reason = ""): void {
    this.readyState = 3;
    this.closedWith = { code, reason };
  }
}

export function fakeSocketFactory(url: string): WebSocket {
  return new FakeSocket(url) as unknown as WebSocket;
}

/** message builds a server frame with the envelope the contract defines. */
export function message(
  type: string,
  seq: number,
  data: unknown,
  sessionId = "00000000-0000-0000-0000-000000000001",
) {
  return { type, seq, ts: new Date().toISOString(), session_id: sessionId, data };
}

export function livePitch(index: number, overrides: Record<string, unknown> = {}) {
  return {
    pitch_id: `pitch-${index}`,
    session_pitch_index: index,
    at_bat_number: 1,
    pitch_number: 1,
    thrown_at: new Date().toISOString(),
    context: { balls: 0, strikes: 0, outs_when_up: 0, inning: 1, on_1b: false, on_2b: false, on_3b: false },
    stand: "R",
    p_throws: "R",
    pitch_type: "FF",
    pitch_name: "4-Seam Fastball",
    measurement: { release_speed: 95, release_spin_rate: 2400, measurement_quality: "OK", normalization_version: 1 },
    predictions: [
      {
        variant: "pitching",
        model_version: "test-v1",
        target: "outcome_5class",
        probabilities: { BALL: 0.2, CALLED_STRIKE: 0.2, SWINGING_STRIKE: 0.3, FOUL: 0.2, IN_PLAY: 0.1 },
        predicted_class: "SWINGING_STRIKE",
        scored_quantity: -0.01,
        score: 105,
      },
    ],
    actual_outcome: "SWINGING_STRIKE",
    prediction_status: "OK",
    is_synthetic: true,
    ...overrides,
  };
}
