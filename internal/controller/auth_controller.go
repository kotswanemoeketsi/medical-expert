package controller

import (
	"net/http"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/service"
)

// userDTO is the user as the API shows it. Explicit rather than the model
// with tags, so adding a column never publishes it by accident.
type userDTO struct {
	ID                model.ID         `json:"id"`
	Email             string           `json:"email"`
	FullName          string           `json:"full_name"`
	Mobile            string           `json:"mobile,omitempty"`
	Role              model.Role       `json:"role"`
	ProfessionalRegNo string           `json:"professional_reg_no,omitempty"`
	PatientID         *model.ID        `json:"patient_id,omitempty"`
	Status            model.UserStatus `json:"status"`
	MFAEnabled        bool             `json:"mfa_enabled"`
	MFAEnrolledAt     *time.Time       `json:"mfa_enrolled_at,omitempty"`
	Locked            bool             `json:"locked"`
	LastLoginAt       *time.Time       `json:"last_login_at,omitempty"`
	CreatedAt         time.Time        `json:"created_at"`
}

func toUserDTO(u *model.User) userDTO {
	return userDTO{
		ID: u.ID, Email: u.Email, FullName: u.FullName, Mobile: u.Mobile, Role: u.Role,
		ProfessionalRegNo: u.ProfessionalRegNo, PatientID: u.PatientID, Status: u.Status,
		MFAEnabled: u.MFAEnabled, MFAEnrolledAt: u.MFAEnrolledAt, Locked: u.IsLocked(time.Now().UTC()),
		LastLoginAt: u.LastLoginAt, CreatedAt: u.CreatedAt,
	}
}

type tenantDTO struct {
	ID       model.ID `json:"id"`
	Slug     string   `json:"slug"`
	Name     string   `json:"name"`
	Timezone string   `json:"timezone"`
}

func toTenantDTO(t *model.Tenant) *tenantDTO {
	if t == nil {
		return nil
	}
	return &tenantDTO{ID: t.ID, Slug: t.Slug, Name: t.Name, Timezone: t.Timezone}
}

type sessionDTO struct {
	State            service.SessionState `json:"state"`
	Token            string               `json:"token"`
	ExpiresAt        time.Time            `json:"expires_at"`
	MFARequired      bool                 `json:"mfa_required"`
	MFASetupRequired bool                 `json:"mfa_setup_required"`
	User             *userDTO             `json:"user,omitempty"`
	Tenant           *tenantDTO           `json:"tenant,omitempty"`
}

func toSessionDTO(res *service.LoginResult) sessionDTO {
	out := sessionDTO{
		State: res.State, Token: res.Token, ExpiresAt: res.ExpiresAt,
		MFARequired: res.State == service.StatePending, MFASetupRequired: res.State == service.StateSetup,
		Tenant: toTenantDTO(res.Tenant),
	}
	if res.User != nil {
		u := toUserDTO(res.User)
		out.User = &u
	}
	return out
}

type loginRequest struct {
	TenantSlug string `json:"tenant_slug"`
	Email      string `json:"email"`
	Password   string `json:"password"`
}

func (a *API) meta(r *http.Request) service.RequestMeta {
	return service.RequestMeta{
		RequestID: requestIDFrom(r.Context()),
		IP:        clientIP(r, a.opt.TrustProxyHeaders),
		UserAgent: r.UserAgent(),
	}
}

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	res, err := a.reg.Login(r.Context(), service.LoginInput{
		TenantSlug: req.TenantSlug, Email: req.Email, Password: req.Password,
	}, a.meta(r))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toSessionDTO(res))
}

func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	if err := principalFrom(r).Services.Session.Logout(r.Context()); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type mfaStatusDTO struct {
	Enabled                bool       `json:"enabled"`
	EnrolledAt             *time.Time `json:"enrolled_at,omitempty"`
	RecoveryCodesRemaining int        `json:"recovery_codes_remaining"`
	Required               bool       `json:"required"`
}

func toMFAStatusDTO(s *service.MFAStatus) mfaStatusDTO {
	return mfaStatusDTO{Enabled: s.Enabled, EnrolledAt: s.EnrolledAt,
		RecoveryCodesRemaining: s.RecoveryCodesRemaining, Required: s.Required}
}

func (a *API) me(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	res, err := p.Services.Session.Me(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":    toUserDTO(res.User),
		"tenant":  toTenantDTO(res.Tenant),
		"session": map[string]any{"state": res.State},
		"mfa":     toMFAStatusDTO(&res.MFA),
	})
}

type passwordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (a *API) changePassword(w http.ResponseWriter, r *http.Request) {
	var req passwordRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	if err := principalFrom(r).Services.Session.ChangePassword(r.Context(), req.CurrentPassword, req.NewPassword); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type verifyRequest struct {
	Code         string `json:"code"`
	RecoveryCode string `json:"recovery_code"`
}

func (a *API) mfaVerify(w http.ResponseWriter, r *http.Request) {
	var req verifyRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	res, err := principalFrom(r).Services.MFA.Verify(r.Context(), service.VerifyInput{Code: req.Code, RecoveryCode: req.RecoveryCode})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toSessionDTO(res))
}

func (a *API) mfaStatus(w http.ResponseWriter, r *http.Request) {
	st, err := principalFrom(r).Services.MFA.Status(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toMFAStatusDTO(st))
}

func (a *API) mfaEnrol(w http.ResponseWriter, r *http.Request) {
	e, err := principalFrom(r).Services.MFA.BeginEnrolment(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"secret": e.Secret, "otpauth_uri": e.URI, "issuer": e.Issuer, "account": e.Account,
	})
}

type codeRequest struct {
	Code string `json:"code"`
}

func (a *API) mfaConfirm(w http.ResponseWriter, r *http.Request) {
	var req codeRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	codes, err := principalFrom(r).Services.MFA.ConfirmEnrolment(r.Context(), req.Code)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "recovery_codes": codes.Codes})
}

func (a *API) mfaRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	var req codeRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	codes, err := principalFrom(r).Services.MFA.RegenerateRecoveryCodes(r.Context(), req.Code)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes.Codes})
}
