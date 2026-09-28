package controller

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
)

// Role groups for routes. super_admin is deliberately absent from all of
// them: a platform operator has no routine access to a practice's data
// (BR-5.1); break-glass access is a separate, audited path.
var (
	anyRole        []model.Role // any authenticated session that passes the MFA gate
	staffRoles     = []model.Role{model.RoleClinicAdmin, model.RoleDoctor, model.RoleNurse}
	clinicalRoles  = []model.Role{model.RoleDoctor, model.RoleNurse}
	adminRoles     = []model.Role{model.RoleClinicAdmin}
	staffOrPatient = []model.Role{model.RoleClinicAdmin, model.RoleDoctor, model.RoleNurse, model.RolePatient}
	clinicalOrSelf = []model.Role{model.RoleDoctor, model.RoleNurse, model.RolePatient}
)

// Route describes one endpoint. Exported so tests can walk every route.
type Route struct {
	Method  string
	Path    string
	Public  bool
	Roles   []model.Role // nil with Public false: any signed-in role
	handler http.HandlerFunc
	limiter *RateLimiter
}

// Routes lists every endpoint the API serves.
func (a *API) Routes() []Route {
	return []Route{
		{Method: "GET", Path: "/healthz", Public: true, handler: a.health},

		// Authentication
		{Method: "POST", Path: "/api/v1/auth/login", Public: true, handler: a.login, limiter: a.loginLimiter},
		{Method: "POST", Path: "/api/v1/auth/logout", Roles: anyRole, handler: a.logout},
		{Method: "GET", Path: "/api/v1/me", Roles: anyRole, handler: a.me},
		{Method: "POST", Path: "/api/v1/auth/password", Roles: anyRole, handler: a.changePassword, limiter: a.loginLimiter},
		{Method: "POST", Path: "/api/v1/auth/mfa/verify", Roles: anyRole, handler: a.mfaVerify, limiter: a.mfaLimiter},
		{Method: "GET", Path: "/api/v1/auth/mfa/status", Roles: anyRole, handler: a.mfaStatus},
		{Method: "POST", Path: "/api/v1/auth/mfa/enrol", Roles: anyRole, handler: a.mfaEnrol},
		{Method: "POST", Path: "/api/v1/auth/mfa/enrol/confirm", Roles: anyRole, handler: a.mfaConfirm, limiter: a.mfaLimiter},
		{Method: "POST", Path: "/api/v1/auth/mfa/recovery-codes", Roles: anyRole, handler: a.mfaRecoveryCodes, limiter: a.mfaLimiter},

		// Practice administration
		{Method: "POST", Path: "/api/v1/users", Roles: adminRoles, handler: a.createUser},
		{Method: "GET", Path: "/api/v1/users", Roles: adminRoles, handler: a.listUsers},
		{Method: "PATCH", Path: "/api/v1/users/{id}/status", Roles: adminRoles, handler: a.setUserStatus},
		{Method: "POST", Path: "/api/v1/users/{id}/mfa/reset", Roles: adminRoles, handler: a.resetUserMFA},
		{Method: "GET", Path: "/api/v1/audit", Roles: adminRoles, handler: a.listAudit},

		// Patients
		{Method: "POST", Path: "/api/v1/patients", Roles: staffRoles, handler: a.createPatient},
		{Method: "GET", Path: "/api/v1/patients", Roles: staffRoles, handler: a.searchPatients},
		{Method: "GET", Path: "/api/v1/patients/{id}", Roles: staffOrPatient, handler: a.getPatient},
		{Method: "PUT", Path: "/api/v1/patients/{id}", Roles: staffRoles, handler: a.updatePatient},
		{Method: "GET", Path: "/api/v1/patients/{id}/summary", Roles: clinicalOrSelf, handler: a.patientSummary},
		{Method: "POST", Path: "/api/v1/patients/{id}/allergies", Roles: clinicalRoles, handler: a.addAllergy},
		{Method: "PATCH", Path: "/api/v1/patients/{id}/allergies/{allergyId}", Roles: clinicalRoles, handler: a.setAllergyStatus},
		{Method: "POST", Path: "/api/v1/patients/{id}/conditions", Roles: clinicalRoles, handler: a.addCondition},
		{Method: "PATCH", Path: "/api/v1/patients/{id}/conditions/{conditionId}", Roles: clinicalRoles, handler: a.updateCondition},
		{Method: "POST", Path: "/api/v1/patients/{id}/consents", Roles: staffRoles, handler: a.recordConsent},
		{Method: "GET", Path: "/api/v1/patients/{id}/consents", Roles: staffOrPatient, handler: a.listConsents},
		{Method: "PATCH", Path: "/api/v1/patients/{id}/review-interval", Roles: clinicalRoles, handler: a.setReviewInterval},
	}
}

// Handler assembles the middleware chain around every route.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range a.Routes() {
		var h http.Handler = rt.handler
		if !rt.Public {
			if rt.Roles != nil {
				h = a.requireRole(rt.Roles, h)
			}
			h = a.requireAuth(h)
		}
		if rt.limiter != nil {
			h = a.rateLimit(rt.limiter, h)
		}
		mux.Handle(rt.Method+" "+rt.Path, h)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, errorEnvelope{Error: map[string]any{
			"code": "not_found", "message": "No such endpoint.", "request_id": requestIDFrom(r.Context()),
		}})
	})
	// Outermost first: every response gets a request ID and the security
	// headers, including the 500 a recovered panic produces.
	return a.withRequestID(a.securityHeaders(a.accessLog(a.recoverer(a.limitBody(mux)))))
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.reg.Ping(ctx); err != nil {
		a.log.WarnContext(r.Context(), "health check: database unreachable", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func slogLevelFor(status int, path string) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case path == "/healthz":
		return slog.LevelDebug
	}
	return slog.LevelInfo
}
