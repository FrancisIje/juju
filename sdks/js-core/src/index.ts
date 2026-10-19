// @juju/core: the platform-independent half of the juju JavaScript SDKs.
// Platform packages (@juju/web, later @juju/react-native) add the hooks
// that notice errors; this package builds, queues and delivers them.

export { Client, VERSION, type ClientOptions } from "./client";
export { Breadcrumbs } from "./breadcrumbs";
export { parseDsn, DsnError, type Dsn } from "./dsn";
export { parseStack, exceptionFromThrown, defaultIsInApp, type InAppFn } from "./stacktrace";
export { fetchTransport, classify, parseRetryAfter, KEEPALIVE_LIMIT, type Transport, type SendResult } from "./transport";
export { uuid4 } from "./uuid";
export type { Breadcrumb, Envelope, ExceptionData, Frame, JujuEvent, SdkInfo, User } from "./types";
