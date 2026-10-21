// @juju/core: the platform-independent half of the juju JavaScript SDKs.
// Platform packages (@juju/web, later @juju/react-native) add the hooks
// that notice errors; this package builds, queues and delivers them.

export { Client, VERSION, SESSION_RESUME_MS, type ClientOptions, type QueueStorage, type KeyValueStorage } from "./client";
export { Transaction, Span, isoTime, monotonicClock, MAX_SPANS, type SpanOptions, type TransactionOptions } from "./tracing";
export { Breadcrumbs } from "./breadcrumbs";
export { parseDsn, DsnError, type Dsn } from "./dsn";
export { parseStack, exceptionFromThrown, defaultIsInApp, type InAppFn } from "./stacktrace";
export {
  fetchTransport,
  classify,
  parseRetryAfter,
  gzip,
  byteLength,
  KEEPALIVE_LIMIT,
  COMPRESS_MIN_BYTES,
  type Transport,
  type SendResult,
  type FetchTransportOptions,
} from "./transport";
export { uuid4 } from "./uuid";
export { instrumentConsole, instrumentXHR, finishHttpSpan, redact, toText, type Uninstall } from "./instrument";
export type {
  AnalyticsEvent,
  Breadcrumb,
  Envelope,
  ExceptionData,
  Frame,
  IdentifyEvent,
  JujuEvent,
  OutgoingEvent,
  SdkInfo,
  SessionData,
  SessionEvent,
  SpanData,
  TransactionEvent,
  User,
} from "./types";
