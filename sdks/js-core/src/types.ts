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

export interface SdkInfo {
  name: string;
  version: string;
}

export interface Envelope {
  sdk: SdkInfo;
  events: JujuEvent[];
}
