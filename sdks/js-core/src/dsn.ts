// A DSN ("Data Source Name") packs everything the SDK needs to reach juju into
// one copy-pasteable string:
//
//   https://pk_ceab7e46...@juju.example.com/1
//           └─ public key ─┘└─── server ───┘└ project id
//
// One value to configure instead of three means fewer ways to get it wrong.

export interface Dsn {
  publicKey: string;
  /** e.g. "https://juju.example.com" */
  origin: string;
  projectId: string;
  /** e.g. "https://juju.example.com/api/v1/projects/1/events" */
  endpoint: string;
}

export class DsnError extends Error {
  override name = "DsnError";
}

// Parsed by hand instead of with new URL(): React Native's URL class is
// incomplete, and reading url.username or url.origin there throws "not
// implemented". The SDK core has to run in both places.
const SCHEME = /^(https?):\/\//;
const DSN = /^(https?):\/\/([^@/\s]+)@([^/\s?#@]+)\/(\d+)\/?$/;

export function parseDsn(dsn: string): Dsn {
  const value = dsn.trim();
  if (!SCHEME.test(value)) {
    if (/^[a-z][a-z0-9+.-]*:\/\//i.test(value)) {
      throw new DsnError("juju: the DSN must start with https:// (or http:// for local development)");
    }
    throw new DsnError(`juju: "${dsn}" is not a valid DSN. It looks like https://pk_...@host/1`);
  }
  const afterScheme = value.replace(SCHEME, "");
  if (!afterScheme.includes("@")) {
    throw new DsnError("juju: the DSN has no public key. It goes before the @, like https://pk_...@host/1");
  }
  const m = DSN.exec(value);
  if (!m) {
    throw new DsnError("juju: the DSN has no project id. It's the number after the host, like https://pk_...@host/1");
  }
  const [, scheme, key, host, projectId] = m as unknown as [string, string, string, string, string];
  const origin = `${scheme}://${host.toLowerCase()}`;
  return {
    publicKey: decodeURIComponent(key),
    origin,
    projectId,
    endpoint: `${origin}/api/v1/projects/${projectId}/events`,
  };
}
