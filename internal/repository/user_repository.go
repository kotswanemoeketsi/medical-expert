package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/tenant"
)

// UserRepository stores logins, their lockout state and second factor.
type UserRepository struct {
	scope *tenant.Scope
}

func NewUserRepository(s *tenant.Scope) *UserRepository { return &UserRepository{scope: s} }

const userColumns = `id, tenant_id, email, full_name, mobile, password_hash, role, professional_reg_no,
       patient_id, status, mfa_secret, mfa_enabled, mfa_last_counter, mfa_enrolled_at,
       failed_attempts, locked_until, last_login_at, created_at, updated_at`

func scanUser(sc scanner) (*model.User, error) {
	var u model.User
	var mobile, regNo sql.Null[string]
	var patientID sql.Null[model.ID]
	var lastCounter sql.Null[uint64]
	var enrolledAt, lockedUntil, lastLogin sql.Null[time.Time]
	err := sc.Scan(&u.ID, &u.TenantID, &u.Email, &u.FullName, &mobile, &u.PasswordHash, &u.Role, &regNo,
		&patientID, &u.Status, &u.MFASecret, &u.MFAEnabled, &lastCounter, &enrolledAt,
		&u.FailedAttempts, &lockedUntil, &lastLogin, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, translate(err)
	}
	u.Mobile, u.ProfessionalRegNo = mobile.V, regNo.V
	u.PatientID = idPtr(patientID)
	if lastCounter.Valid {
		c := lastCounter.V
		u.MFALastCounter = &c
	}
	u.MFAEnrolledAt, u.LockedUntil, u.LastLoginAt = timePtr(enrolledAt), timePtr(lockedUntil), timePtr(lastLogin)
	return &u, nil
}

const insertUser = `
INSERT INTO users (tenant_id, id, email, full_name, mobile, password_hash, role, professional_reg_no,
                   patient_id, status, created_by, created_at, updated_at, password_changed_at)
VALUES (:tenant, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// Create inserts a user. A taken email returns a *DuplicateKeyError for
// uq_users_email; a patient that already has a login, for uq_users_patient.
func (r *UserRepository) Create(ctx context.Context, u *model.User, createdBy *model.ID) error {
	_, err := r.scope.Exec(ctx, insertUser,
		u.ID.Bytes(), u.Email, u.FullName, nullString(u.Mobile), u.PasswordHash, string(u.Role),
		nullString(u.ProfessionalRegNo), nullID(u.PatientID), string(u.Status), nullID(createdBy),
		u.CreatedAt, u.UpdatedAt, u.CreatedAt)
	return translate(err)
}

const selectUserByID = `SELECT ` + userColumns + ` FROM users WHERE tenant_id = :tenant AND id = ?`

func (r *UserRepository) GetByID(ctx context.Context, id model.ID) (*model.User, error) {
	return scanUser(r.scope.QueryRow(ctx, selectUserByID, id.Bytes()))
}

const selectUserByEmail = `SELECT ` + userColumns + ` FROM users WHERE tenant_id = :tenant AND email = ?`

func (r *UserRepository) GetByEmail(ctx context.Context, email string) (*model.User, error) {
	return scanUser(r.scope.QueryRow(ctx, selectUserByEmail, email))
}

const selectUsers = `SELECT ` + userColumns + ` FROM users WHERE tenant_id = :tenant ORDER BY full_name, email`

func (r *UserRepository) List(ctx context.Context) ([]model.User, error) {
	rows, err := r.scope.Query(ctx, selectUsers)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

const updateUserStatus = `UPDATE users SET status = ?, updated_at = ? WHERE tenant_id = :tenant AND id = ?`

func (r *UserRepository) UpdateStatus(ctx context.Context, id model.ID, status model.UserStatus, now time.Time) error {
	return expectFound(r.scope.Exec(ctx, updateUserStatus, string(status), now, id.Bytes()))
}

const updatePasswordHash = `
UPDATE users SET password_hash = ?, password_changed_at = ?, updated_at = ?
 WHERE tenant_id = :tenant AND id = ?`

func (r *UserRepository) UpdatePasswordHash(ctx context.Context, id model.ID, hash string, now time.Time) error {
	return expectFound(r.scope.Exec(ctx, updatePasswordHash, hash, now, now, id.Bytes()))
}

// --- lockout (BR-1.6) -------------------------------------------------------

const clearLapsedLock = `
UPDATE users SET failed_attempts = 0, locked_until = NULL
 WHERE tenant_id = :tenant AND id = ? AND locked_until IS NOT NULL AND locked_until <= ?`

// locked_until is assigned first and reads the old failed_attempts, so the
// statement means the same whether the server evaluates assignments left to
// right (MySQL) or simultaneously (MariaDB's SIMULTANEOUS_ASSIGNMENT mode).
const incrementFailedAttempts = `
UPDATE users
   SET locked_until = CASE WHEN failed_attempts + 1 >= ? THEN ? ELSE locked_until END,
       failed_attempts = failed_attempts + 1
 WHERE tenant_id = :tenant AND id = ?`

const selectLockState = `SELECT failed_attempts, locked_until FROM users WHERE tenant_id = :tenant AND id = ?`

// RecordFailedAttempt counts a failed password or second-factor attempt and
// locks the account for lockout once maxAttempts is reached. A lock that has
// already lapsed is cleared first, so the count starts again rather than
// re-locking on the first mistake after a lockout. Returns whether the
// account is now locked.
func (r *UserRepository) RecordFailedAttempt(ctx context.Context, id model.ID, now time.Time, maxAttempts int, lockout time.Duration) (locked bool, err error) {
	err = r.scope.InTx(ctx, func(tx *tenant.Scope) error {
		if _, err := tx.Exec(ctx, clearLapsedLock, id.Bytes(), now); err != nil {
			return err
		}
		if err := expectFound(tx.Exec(ctx, incrementFailedAttempts, maxAttempts, now.Add(lockout), id.Bytes())); err != nil {
			return err
		}
		var attempts int
		var until sql.Null[time.Time]
		if err := tx.QueryRow(ctx, selectLockState, id.Bytes()).Scan(&attempts, &until); err != nil {
			return translate(err)
		}
		locked = until.Valid && until.V.After(now)
		return nil
	})
	return locked, err
}

const clearLockout = `UPDATE users SET failed_attempts = 0, locked_until = NULL WHERE tenant_id = :tenant AND id = ?`

// ClearLockout lets an administrator unlock an account before the lockout
// lapses on its own.
func (r *UserRepository) ClearLockout(ctx context.Context, id model.ID) error {
	return expectFound(r.scope.Exec(ctx, clearLockout, id.Bytes()))
}

const recordLogin = `
UPDATE users SET failed_attempts = 0, locked_until = NULL, last_login_at = ?
 WHERE tenant_id = :tenant AND id = ?`

// RecordSuccessfulLogin clears the failure count. Called only once sign-in
// is complete — after the second factor when there is one — or a stolen
// password would reset the count between guesses at the code.
func (r *UserRepository) RecordSuccessfulLogin(ctx context.Context, id model.ID, now time.Time) error {
	return expectFound(r.scope.Exec(ctx, recordLogin, now, id.Bytes()))
}

// --- second factor ------------------------------------------------------------

const setMFASecret = `
UPDATE users SET mfa_secret = ?, mfa_last_counter = NULL, updated_at = ?
 WHERE tenant_id = :tenant AND id = ? AND mfa_enabled = 0`

// SetMFASecret stores a secret for an enrolment in progress. Refused with
// ErrConflict once MFA is on: replacing a live secret is a reset, which is an
// administrator's decision.
func (r *UserRepository) SetMFASecret(ctx context.Context, id model.ID, secret []byte, now time.Time) error {
	return expectOne(r.scope.Exec(ctx, setMFASecret, secret, now, id.Bytes()))
}

const enableMFA = `
UPDATE users SET mfa_enabled = 1, mfa_enrolled_at = ?, mfa_last_counter = ?, updated_at = ?
 WHERE tenant_id = :tenant AND id = ? AND mfa_enabled = 0 AND mfa_secret IS NOT NULL`

// EnableMFA switches the factor on, recording the counter of the code that
// proved the secret reached the device so that code cannot be used again.
func (r *UserRepository) EnableMFA(ctx context.Context, id model.ID, counter uint64, now time.Time) error {
	return expectOne(r.scope.Exec(ctx, enableMFA, now, counter, now, id.Bytes()))
}

// The replay guard lives in the WHERE clause: of two requests presenting the
// same code at the same moment, exactly one matches.
const advanceMFACounter = `
UPDATE users SET mfa_last_counter = ?
 WHERE tenant_id = :tenant AND id = ? AND mfa_enabled = 1
   AND (mfa_last_counter IS NULL OR mfa_last_counter < ?)`

// AdvanceMFACounter spends a code. ErrConflict means it was already spent.
func (r *UserRepository) AdvanceMFACounter(ctx context.Context, id model.ID, counter uint64) error {
	return expectOne(r.scope.Exec(ctx, advanceMFACounter, counter, id.Bytes(), counter))
}

const resetMFA = `
UPDATE users SET mfa_secret = NULL, mfa_enabled = 0, mfa_last_counter = NULL, mfa_enrolled_at = NULL, updated_at = ?
 WHERE tenant_id = :tenant AND id = ?`

const deleteRecoveryCodes = `DELETE FROM user_recovery_codes WHERE tenant_id = :tenant AND user_id = ?`

// ResetMFA removes the factor and every recovery code.
func (r *UserRepository) ResetMFA(ctx context.Context, id model.ID, now time.Time) error {
	return r.scope.InTx(ctx, func(tx *tenant.Scope) error {
		if err := expectFound(tx.Exec(ctx, resetMFA, now, id.Bytes())); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, deleteRecoveryCodes, id.Bytes())
		return err
	})
}

const insertRecoveryCode = `
INSERT INTO user_recovery_codes (tenant_id, id, user_id, code_hash, created_at)
VALUES (:tenant, ?, ?, ?, ?)`

// ReplaceRecoveryCodes invalidates every existing code and stores the new set.
func (r *UserRepository) ReplaceRecoveryCodes(ctx context.Context, userID model.ID, hashes [][32]byte, now time.Time) error {
	return r.scope.InTx(ctx, func(tx *tenant.Scope) error {
		if _, err := tx.Exec(ctx, deleteRecoveryCodes, userID.Bytes()); err != nil {
			return err
		}
		for _, h := range hashes {
			id, err := model.NewID()
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, insertRecoveryCode, id.Bytes(), userID.Bytes(), h[:], now); err != nil {
				return translate(err)
			}
		}
		return nil
	})
}

const useRecoveryCode = `
UPDATE user_recovery_codes SET used_at = ?
 WHERE tenant_id = :tenant AND user_id = ? AND code_hash = ? AND used_at IS NULL`

// UseRecoveryCode spends a code. ErrConflict means no unused code matched.
func (r *UserRepository) UseRecoveryCode(ctx context.Context, userID model.ID, hash [32]byte, now time.Time) error {
	return expectOne(r.scope.Exec(ctx, useRecoveryCode, now, userID.Bytes(), hash[:]))
}

const countRecoveryCodes = `
SELECT COUNT(*) FROM user_recovery_codes WHERE tenant_id = :tenant AND user_id = ? AND used_at IS NULL`

func (r *UserRepository) CountUnusedRecoveryCodes(ctx context.Context, userID model.ID) (int, error) {
	var n int
	err := r.scope.QueryRow(ctx, countRecoveryCodes, userID.Bytes()).Scan(&n)
	return n, translate(err)
}
