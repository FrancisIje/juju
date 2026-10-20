package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrInviteInvalid: the invite doesn't exist, was used, was revoked or has
// expired. One error for all of them: someone holding a link can't tell
// those apart, and doesn't need to.
var ErrInviteInvalid = errors.New("invite expired or already used")

// ErrLastOwner: removing this person would leave nobody able to manage the
// organization.
var ErrLastOwner = errors.New("last owner")

type Invite struct {
	ID        int64     `json:"id"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// InviteInfo is what the invite page shows before someone accepts.
type InviteInfo struct {
	OrganizationID int64     `json:"organization_id"`
	Organization   string    `json:"organization"`
	InvitedBy      string    `json:"invited_by"`
	ExpiresAt      time.Time `json:"expires_at"`
}

type Member struct {
	UserID   int64     `json:"user_id"`
	Email    string    `json:"email"`
	Name     string    `json:"name"`
	Role     string    `json:"role"`
	JoinedAt time.Time `json:"joined_at"`
}

func (s *Store) CreateInvite(ctx context.Context, orgID, createdBy int64, tokenHash []byte, expires time.Time) (Invite, error) {
	inv := Invite{ExpiresAt: expires}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO invites (organization_id, token_hash, created_by, expires_at) VALUES ($1, $2, $3, $4)
		RETURNING id, created_at, coalesce((SELECT coalesce(nullif(name, ''), email) FROM users WHERE id = $3), '')`,
		orgID, tokenHash, createdBy, expires).Scan(&inv.ID, &inv.CreatedAt, &inv.CreatedBy)
	return inv, err
}

// PendingInvites are unused and unexpired, newest first.
func (s *Store) PendingInvites(ctx context.Context, orgID int64) ([]Invite, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, coalesce(nullif(u.name, ''), u.email, ''), i.created_at, i.expires_at
		FROM invites i LEFT JOIN users u ON u.id = i.created_by
		WHERE i.organization_id = $1 AND i.used_at IS NULL AND i.expires_at > now()
		ORDER BY i.created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	out := []Invite{}
	for rows.Next() {
		var inv Invite
		if err := rows.Scan(&inv.ID, &inv.CreatedBy, &inv.CreatedAt, &inv.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// RevokeInvite deletes an invite of this organization. False if there was
// none (the id belongs to another organization, or was already used).
func (s *Store) RevokeInvite(ctx context.Context, orgID, inviteID int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM invites WHERE id = $1 AND organization_id = $2 AND used_at IS NULL`, inviteID, orgID)
	return tag.RowsAffected() == 1, err
}

func (s *Store) InviteByToken(ctx context.Context, tokenHash []byte) (InviteInfo, error) {
	var info InviteInfo
	err := s.pool.QueryRow(ctx, `
		SELECT o.id, o.name, coalesce(nullif(u.name, ''), u.email, ''), i.expires_at
		FROM invites i JOIN organizations o ON o.id = i.organization_id
		LEFT JOIN users u ON u.id = i.created_by
		WHERE i.token_hash = $1 AND i.used_at IS NULL AND i.expires_at > now()`, tokenHash).
		Scan(&info.OrganizationID, &info.Organization, &info.InvitedBy, &info.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return info, ErrInviteInvalid
	}
	return info, err
}

// claim marks the invite used by userID and adds the membership, inside tx.
// The UPDATE ... WHERE used_at IS NULL is what makes a link work once: of
// two people accepting at the same moment, only one UPDATE finds the row.
func claim(ctx context.Context, tx pgx.Tx, tokenHash []byte, userID int64) (int64, error) {
	var orgID int64
	var role string
	err := tx.QueryRow(ctx, `
		UPDATE invites SET used_at = now(), used_by = $2
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
		RETURNING organization_id, role`, tokenHash, userID).Scan(&orgID, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrInviteInvalid
	}
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO memberships (user_id, organization_id, role) VALUES ($1, $2, $3)`, userID, orgID, role)
	return orgID, err
}

// AcceptInvite adds an existing user to the invite's organization. Someone
// who is already a member gets already=true, and the link stays unused for
// whoever it was really meant for.
func (s *Store) AcceptInvite(ctx context.Context, tokenHash []byte, userID int64) (orgID int64, already bool, err error) {
	info, err := s.InviteByToken(ctx, tokenHash)
	if err != nil {
		return 0, false, err
	}
	if _, err := s.OrgRole(ctx, userID, info.OrganizationID); err == nil {
		return info.OrganizationID, true, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx)
	if orgID, err = claim(ctx, tx, tokenHash, userID); err != nil {
		return 0, false, err
	}
	return orgID, false, tx.Commit(ctx)
}

// CreateUserWithInvite signs someone up straight into the invite's
// organization, in one transaction: an invalid invite creates no account.
func (s *Store) CreateUserWithInvite(ctx context.Context, email, name, passwordHash string, tokenHash []byte) (User, int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, 0, err
	}
	defer tx.Rollback(ctx)
	u := User{Email: email, Name: name}
	err = tx.QueryRow(ctx, `
		INSERT INTO users (email, name, password_hash) VALUES ($1, $2, $3)
		ON CONFLICT (email) DO NOTHING
		RETURNING id, created_at`, email, name, passwordHash).Scan(&u.ID, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, 0, ErrConflict
	}
	if err != nil {
		return User{}, 0, err
	}
	orgID, err := claim(ctx, tx, tokenHash, u.ID)
	if err != nil {
		return User{}, 0, err
	}
	return u, orgID, tx.Commit(ctx)
}

func (s *Store) Members(ctx context.Context, orgID int64) ([]Member, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.email, u.name, m.role, m.created_at
		FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.organization_id = $1
		ORDER BY m.role DESC, m.created_at`, orgID) // owners first ('owner' > 'member')
	if err != nil {
		return nil, err
	}
	out := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Email, &m.Name, &m.Role, &m.JoinedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RemoveMember takes someone out of the organization. It refuses to remove
// the last owner. The row lock stops two owners removing each other at once
// and leaving none.
func (s *Store) RemoveMember(ctx context.Context, orgID, userID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var owners int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM (SELECT 1 FROM memberships WHERE organization_id = $1 AND role = 'owner' FOR UPDATE) o`, orgID).Scan(&owners); err != nil {
		return err
	}
	var role string
	err = tx.QueryRow(ctx, `SELECT role FROM memberships WHERE organization_id = $1 AND user_id = $2`, orgID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if role == "owner" && owners <= 1 {
		return ErrLastOwner
	}
	if _, err := tx.Exec(ctx, `DELETE FROM memberships WHERE organization_id = $1 AND user_id = $2`, orgID, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
