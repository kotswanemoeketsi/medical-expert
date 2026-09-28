package controller

import (
	"net/http"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/service"
)

type createUserRequest struct {
	Email             string     `json:"email"`
	FullName          string     `json:"full_name"`
	Role              model.Role `json:"role"`
	ProfessionalRegNo string     `json:"professional_reg_no"`
	Mobile            string     `json:"mobile"`
	PatientID         *model.ID  `json:"patient_id"`
	Password          string     `json:"password"`
}

func (a *API) createUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	u, err := principalFrom(r).Services.Users.Create(r.Context(), service.CreateUserInput{
		Email: req.Email, FullName: req.FullName, Role: req.Role, ProfessionalRegNo: req.ProfessionalRegNo,
		Mobile: req.Mobile, PatientID: req.PatientID, Password: req.Password,
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toUserDTO(u))
}

func (a *API) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := principalFrom(r).Services.Users.List(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]userDTO, len(users))
	for i := range users {
		out[i] = toUserDTO(&users[i])
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

type statusRequest struct {
	Status model.UserStatus `json:"status"`
	Reason string           `json:"reason"`
}

func (a *API) setUserStatus(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req statusRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	u, err := principalFrom(r).Services.Users.SetStatus(r.Context(), id, req.Status, req.Reason)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toUserDTO(u))
}

type reasonRequest struct {
	Reason string `json:"reason"`
}

func (a *API) resetUserMFA(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req reasonRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	if err := principalFrom(r).Services.Users.ResetMFA(r.Context(), id, req.Reason); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) listAudit(w http.ResponseWriter, r *http.Request) {
	subject, err := queryID(r, "subject_id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	actor, err := queryID(r, "actor_id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var since time.Time
	if s := r.URL.Query().Get("since"); s != "" {
		if since, err = time.Parse(time.RFC3339, s); err != nil {
			a.fail(w, r, &service.Error{Kind: service.KindInvalid, Code: "validation_failed", Message: "Some fields need attention.",
				Fields: []model.FieldError{{Field: "since", Message: "must be an RFC 3339 timestamp"}}})
			return
		}
	}
	before := queryInt(r, "before", 0)
	if before < 0 {
		before = 0
	}
	recs, err := principalFrom(r).Services.Audit.List(r.Context(), service.AuditQuery{
		SubjectID: subject, ActorID: actor, Action: r.URL.Query().Get("action"), Since: since,
		BeforeSeq: uint64(before), Limit: queryInt(r, "limit", 100),
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	resp := map[string]any{"items": recs}
	if len(recs) > 0 {
		resp["next_before"] = recs[len(recs)-1].Seq
	}
	writeJSON(w, http.StatusOK, resp)
}
