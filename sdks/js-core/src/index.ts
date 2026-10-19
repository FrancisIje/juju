// @juju/core: the platform-independent half of the juju JavaScript SDKs.
// Platform packages (@juju/web, later @juju/react-native) add the hooks
// that notice errors; this package builds, queues and delivers them.

export { Client, VERSION, type ClientOptions, type QueueStorage } from "./client";
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
export { instrumentConsole, instrumentXHR, redact, toText, type Uninstall } from "./instrument";
export type { Breadcrumb, Envelope, ExceptionData, Frame, JujuEvent, SdkInfo, User } from "./types";
