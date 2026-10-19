// event_id must be created when the error happens and reused on every retry:
// that's what lets the server recognize a retry and store the event once.

/** Returns a random version 4 UUID. */
export function uuid4(): string {
  const c = (globalThis as { crypto?: Crypto }).crypto;
  // crypto.randomUUID only exists on secure pages (https or localhost), so it
  // isn't enough on its own; getRandomValues works everywhere it exists.
  if (c && typeof c.randomUUID === "function") {
    try {
      return c.randomUUID();
    } catch {
      // fall through: insecure context
    }
  }
  const bytes = new Uint8Array(16);
  if (c && typeof c.getRandomValues === "function") {
    c.getRandomValues(bytes);
  } else {
    // Last resort for old runtimes. Not cryptographically strong, but event
    // ids only need to be unique, not secret.
    for (let i = 0; i < 16; i++) bytes[i] = Math.floor(Math.random() * 256);
  }
  bytes[6] = (bytes[6]! & 0x0f) | 0x40; // version 4
  bytes[8] = (bytes[8]! & 0x3f) | 0x80; // RFC 4122 variant
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}
