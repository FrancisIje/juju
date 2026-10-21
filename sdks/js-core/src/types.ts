// The v1 wire format, mirroring server/internal/event/event.go. If the two
// disagree, the server's validator wins and the event is dropped, so keep
// them in sync.

export interface Frame {
  function?: string;
  filename?: string;
  lineno?: number;
  colno?: number;
  in_app: boolean;
}

export interface ExceptionData {
  type: string;
  message: string;
  /** Frames are ordered most recent call first: frames[0] is where it was thrown. */
  stacktrace?: { frames: Frame[] };
}

export interface Breadcrumb {
  type: string;
  message?: string;
  timestamp: string;
  data?: Record<string, unknown>;
}

export interface User {
  id?: string;
  email?: string;
  username?: string;
}

export interface JujuEvent {
  event_id: string;
  type: "error";
  timestamp: string;
  platform: string;
  release?: string;
  environment?: string;
  exception: ExceptionData;
  breadcrumbs?: Breadcrumb[];
  context?: Record<string, unknown>;
  user?: User;
  tags?: Record<string, string>;
}

/** One update to an app session. See server/internal/event/performance.go. */
export interface SessionData {
  sid: string;
  /** Increases with every update, so the server can tell a late update from a new one. */
  seq: number;
  /** "crashed" is final: the server ignores anything after it. */
  status: "ok" | "exited" | "crashed";
  started: string;
  errors: number;
  duration_ms?: number;
}

export interface SessionEvent {
  event_id: string;
  type: "session";
  timestamp: string;
  platform: string;
  release?: string;
  environment?: string;
  user?: User;
  session: SessionData;
}

export interface SpanData {
  span_id: string;
  parent_span_id?: string;
  op: string;
  description?: string;
  status?: string;
  start_timestamp: string;
  timestamp: string;
  data?: Record<string, unknown>;
}

export interface TransactionEvent {
  event_id: string;
  type: "transaction";
  /** When the transaction ended. */
  timestamp: string;
  platform: string;
  release?: string;
  environment?: string;
  user?: User;
  tags?: Record<string, string>;
  context?: Record<string, unknown>;
  transaction: {
    name: string;
    op: string;
    status: string;
    start_timestamp: string;
    measurements?: Record<string, number>;
    spans?: SpanData[];
  };
}

/** Everything the SDK queues and sends. */
export type OutgoingEvent = JujuEvent | SessionEvent | TransactionEvent;

export interface SdkInfo {
  name: string;
  version: string;
}

export interface Envelope {
  sdk: SdkInfo;
  events: OutgoingEvent[];
}
