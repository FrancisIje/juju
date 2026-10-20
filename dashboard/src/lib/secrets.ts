// A secret key is shown once: right after the project is created or the key
// is rotated. It's handed from page to page in memory only, never written
// to browser storage, so it's gone after a reload, like on the server.
const pending = new Map<number, string>();

export function rememberSecret(projectId: number, secret: string) {
  pending.set(projectId, secret);
}

export function takeSecret(projectId: number): string | undefined {
  const s = pending.get(projectId);
  pending.delete(projectId);
  return s;
}
