package store

import (
	"context"
	"errors"

	"github.com/hoarfrost/nebulaflow/internal/database"
	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type UserRepo struct{ db *database.DB }

func (r *UserRepo) CreateUser(ctx context.Context, u *model.User) error {
	role := u.Role
	if role == "" {
		role = model.RoleUser
	}
	err := r.db.Pool.QueryRow(ctx,
		`INSERT INTO users (username, email, password_hash, role) VALUES ($1, $2, $3, $4)
		 RETURNING id, created_at, updated_at`,
		u.Username, u.Email, u.PasswordHash, role,
	).Scan(&u.ID, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
			return ErrUserExists
		}
		return err
	}
	u.Role = role
	return nil
}

func (r *UserRepo) GetUserByUsername(ctx context.Context, username string) (*model.User, error) {
	u := &model.User{}
	err := r.db.Pool.QueryRow(ctx,
		`SELECT id, username, email, password_hash, role, created_at, updated_at
		 FROM users WHERE username = $1`, username,
	).Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash, &u.Role, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (r *UserRepo) GetUserByID(ctx context.Context, id int64) (*model.User, error) {
	u := &model.User{}
	err := r.db.Pool.QueryRow(ctx,
		`SELECT id, username, email, password_hash, role, created_at, updated_at
		 FROM users WHERE id = $1`, id,
	).Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash, &u.Role, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (r *UserRepo) GetUserRole(ctx context.Context, userID int64) (string, error) {
	var role string
	err := r.db.Pool.QueryRow(ctx,
		`SELECT role FROM users WHERE id = $1`, userID,
	).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUserNotFound
	}
	if err != nil {
		return "", err
	}
	return role, nil
}

func (r *UserRepo) SetUserRole(ctx context.Context, userID int64, role string) error {
	ct, err := r.db.Pool.Exec(ctx,
		`UPDATE users SET role = $1, updated_at = now() WHERE id = $2`, role, userID,
	)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

func (r *UserRepo) CountUsers(ctx context.Context) (int64, error) {
	var count int64
	err := r.db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}
