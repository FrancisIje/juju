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

export function parseDsn(dsn: string): Dsn {
  let url: URL;
  try {
    url = new URL(dsn.trim());
  } catch {
    throw new DsnError(`juju: "${dsn}" is not a valid DSN. It looks like https://pk_...@host/1`);
  }
  if (url.protocol !== "https:" && url.protocol !== "http:") {
    throw new DsnError("juju: the DSN must start with https:// (or http:// for local development)");
  }
  const publicKey = decodeURIComponent(url.username);
  if (!publicKey) {
    throw new DsnError("juju: the DSN has no public key. It goes before the @, like https://pk_...@host/1");
  }
  const projectId = url.pathname.replace(/^\/+|\/+$/g, "");
  if (!/^\d+$/.test(projectId)) {
    throw new DsnError("juju: the DSN has no project id. It's the number after the host, like https://pk_...@host/1");
  }
  return {
    publicKey,
    origin: url.origin,
    projectId,
    endpoint: `${url.origin}/api/v1/projects/${projectId}/events`,
  };
}
