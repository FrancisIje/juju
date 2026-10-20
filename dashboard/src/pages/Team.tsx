import { useState } from "react";
import { Code } from "../components/CopyButton";
import { api, ApiError, type Org, type PendingInvite, type TeamView } from "../lib/api";
import { useApp } from "../lib/app";
import { exact, relative, until } from "../lib/time";
import { useApi } from "../lib/useApi";

// Settings → Team: who can see this organization's projects, and how to
// add or remove people.
export function Team({ org }: { org: Org }) {
  const { toast } = useApp();
  const { data, error, reload, setData } = useApi<TeamView>(`/api/ui/orgs/${org.id}/members`);
  const [link, setLink] = useState("");
  const [busy, setBusy] = useState(false);
  const [removing, setRemoving] = useState<number | null>(null);

  async function createInvite() {
    setBusy(true);
    try {
      const r = await api<{ url: string; invite: PendingInvite }>(`/api/ui/orgs/${org.id}/invites`, { method: "POST" });
      setLink(r.url);
      if (data) setData({ ...data, invites: [r.invite, ...(data.invites ?? [])] });
    } catch (err) {
      toast(err instanceof ApiError ? err.message : "Couldn't create the invite.");
    } finally {
      setBusy(false);
    }
  }

  async function revoke(id: number) {
    try {
      await api(`/api/ui/orgs/${org.id}/invites/${id}`, { method: "DELETE" });
      toast("Invite link revoked. It no longer works.");
      void reload(true);
    } catch (err) {
      toast(err instanceof ApiError ? err.message : "Couldn't revoke the invite.");
    }
  }

  async function remove(userId: number, who: string) {
    try {
      await api(`/api/ui/orgs/${org.id}/members/${userId}`, { method: "DELETE" });
      toast(`${who} was removed from ${org.name}`);
      setRemoving(null);
      void reload(true);
    } catch (err) {
      toast(err instanceof ApiError ? err.message : "Couldn't remove them.");
    }
  }

  if (error) return <div className="banner warn" role="alert">{error.message}</div>;
  if (!data) return <p className="muted">Loading…</p>;
  const owner = data.your_role === "owner";
  const owners = data.members.filter((m) => m.role === "owner");

  return (
    <div style={{ display: "grid", gap: "var(--space-5)" }}>
      <section className="panel" style={{ padding: "var(--space-5)", display: "grid", gap: "var(--space-3)" }}>
        <div style={{ display: "grid", gap: 4 }}>
          <h2>Invite people to {org.name}</h2>
          <p className="muted">Everyone in {org.name} can see and work on all of its projects. Owners can also invite and remove people.</p>
        </div>
        {owner ? (
          <>
            {link ? (
              <div className="banner ok" role="status" style={{ display: "grid", gap: "var(--space-2)" }}>
                <strong>Send this link to the person you're inviting. It's shown only now.</strong>
                <span>It works once and expires in 7 days. Anyone who has it can join, so send it only to them.</span>
                <Code>{link}</Code>
              </div>
            ) : null}
            <button className="btn primary" style={{ justifySelf: "start" }} onClick={createInvite} disabled={busy}>
              {link ? "Create another link" : "Create invite link"}
            </button>
          </>
        ) : (
          <p className="muted">
            Only owners can invite people. Ask {owners.map((o) => o.name || o.email).join(" or ")}.
          </p>
        )}
      </section>

      <section className="panel" style={{ overflow: "hidden" }}>
        <h2 style={{ padding: "var(--space-4) var(--space-4) var(--space-2)" }}>
          People <span className="muted" style={{ fontWeight: 400 }}>· {data.members.length}</span>
        </h2>
        <div style={{ overflowX: "auto" }}>
          <table className="table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Role</th>
                <th>Joined</th>
                {owner && <th aria-label="Actions" />}
              </tr>
            </thead>
            <tbody>
              {data.members.map((m) => {
                const who = m.name || m.email;
                return (
                  <tr key={m.user_id}>
                    <td>
                      <div>{who}{m.user_id === data.you && <span className="faint"> (you)</span>}</div>
                      {m.name && <div className="faint" style={{ fontSize: "var(--text-xs)" }}>{m.email}</div>}
                    </td>
                    <td>
                      <span className={`chip${m.role === "owner" ? " brand" : ""}`}>{m.role === "owner" ? "Owner" : "Member"}</span>
                    </td>
                    <td title={exact(m.joined_at)}>{relative(m.joined_at)}</td>
                    {owner && (
                      <td className="num">
                        {m.user_id === data.you ? null : removing === m.user_id ? (
                          <span className="toolbar" style={{ justifyContent: "flex-end" }}>
                            <button className="btn danger small" onClick={() => remove(m.user_id, who)}>
                              Remove {who}
                            </button>
                            <button className="btn ghost small" onClick={() => setRemoving(null)}>
                              Cancel
                            </button>
                          </span>
                        ) : (
                          <button className="btn ghost small" onClick={() => setRemoving(m.user_id)}>
                            Remove…
                          </button>
                        )}
                      </td>
                    )}
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </section>

      {owner && (
        <section className="panel" style={{ overflow: "hidden" }}>
          <h2 style={{ padding: "var(--space-4) var(--space-4) var(--space-2)" }}>Open invite links</h2>
          {!data.invites?.length ? (
            <p className="muted" style={{ padding: "0 var(--space-4) var(--space-4)" }}>
              None. Links that were used, revoked or expired disappear from here.
            </p>
          ) : (
            <div style={{ overflowX: "auto" }}>
              <table className="table">
                <thead>
                  <tr>
                    <th>Created by</th>
                    <th>Created</th>
                    <th>Expires</th>
                    <th aria-label="Actions" />
                  </tr>
                </thead>
                <tbody>
                  {data.invites.map((inv) => (
                    <tr key={inv.id}>
                      <td>{inv.created_by || "someone who left"}</td>
                      <td title={exact(inv.created_at)}>{relative(inv.created_at)}</td>
                      <td title={exact(inv.expires_at)}>{until(inv.expires_at)}</td>
                      <td className="num">
                        <button className="btn ghost small" onClick={() => revoke(inv.id)}>
                          Revoke
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </section>
      )}
    </div>
  );
}
