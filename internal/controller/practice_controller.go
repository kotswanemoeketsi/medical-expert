package controller

import (
	"errors"
	"io"
	"net/http"

	"github.com/yourorg/medical-expert-system/internal/notify"
	"github.com/yourorg/medical-expert-system/internal/service"
)

// ---------------------------------------------------------------------------
// Practice details and settings
// ---------------------------------------------------------------------------

func (a *API) getPractice(w http.ResponseWriter, r *http.Request) {
	v, err := principalFrom(r).Services.Practice.Get(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

type practiceRequest struct {
	Name            *string `json:"name"`
	PracticeNumber  *string `json:"practice_number"`
	Address         *string `json:"address"`
	Phone           *string `json:"phone"`
	Email           *string `json:"email"`
	WhatsAppEnabled *bool   `json:"whatsapp_enabled"`
	MessagingName   *string `json:"messaging_name"`
	FollowUpMinutes *int    `json:"follow_up_minutes"`
}

func (a *API) updatePractice(w http.ResponseWriter, r *http.Request) {
	var req practiceRequest
	if err := decode(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	v, err := principalFrom(r).Services.Practice.Update(r.Context(), service.PracticeUpdate{
		Name: req.Name, PracticeNumber: req.PracticeNumber, Address: req.Address, Phone: req.Phone, Email: req.Email,
		WhatsAppEnabled: req.WhatsAppEnabled, MessagingName: req.MessagingName, FollowUpMinutes: req.FollowUpMinutes,
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ---------------------------------------------------------------------------
// Dashboard and recall lists
// ---------------------------------------------------------------------------

func (a *API) dashboard(w http.ResponseWriter, r *http.Request) {
	d, err := principalFrom(r).Services.Dashboard.Get(r.Context(), queryInt(r, "days", 0))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (a *API) recalls(w http.ResponseWriter, r *http.Request) {
	list, err := principalFrom(r).Services.Recalls.List(r.Context(), service.RecallReason(r.URL.Query().Get("reason")),
		service.RecallOptions{Days: queryInt(r, "days", 0), Threshold: queryInt(r, "threshold", 0),
			MinDoses: queryInt(r, "min_doses", 0), Limit: queryInt(r, "limit", 0)})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

func (a *API) sendReminder(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	n, err := principalFrom(r).Services.Notifications.SendReminder(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, n)
}

func (a *API) sendRecall(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	n, err := principalFrom(r).Services.Notifications.SendRecall(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, n)
}

func (a *API) patientMessages(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	m, err := principalFrom(r).Services.Notifications.History(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (a *API) messagingHealth(w http.ResponseWriter, r *http.Request) {
	h, err := principalFrom(r).Services.Notifications.Health(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (a *API) closeOverdueDoses(w http.ResponseWriter, r *http.Request) {
	n, err := principalFrom(r).Services.Maintenance.CloseOverdueDoses(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"closed": n})
}

// ---------------------------------------------------------------------------
// WhatsApp webhook: public, because Meta calls it, and signed.
// ---------------------------------------------------------------------------

// whatsappSubscribe answers Meta's one-off check when the webhook is set up.
func (a *API) whatsappSubscribe(w http.ResponseWriter, r *http.Request) {
	challenge, ok := notify.VerifySubscription(r.URL.Query(), a.opt.WhatsAppVerifyToken)
	if !ok {
		writeJSON(w, http.StatusForbidden, errorEnvelope{Error: map[string]any{
			"code": "forbidden", "message": "Verification failed.", "request_id": requestIDFrom(r.Context()),
		}})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, challenge)
}

// whatsappWebhook receives delivery receipts and patients' replies. The
// signature is checked against the raw body before anything is parsed, and
// with no app secret configured every request is refused rather than
// trusted unsigned: this is the only thing between the internet and the
// dose record.
func (a *API) whatsappWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		a.fail(w, r, &badRequest{"The request body could not be read."})
		return
	}
	if err := notify.VerifySignature(a.opt.WhatsAppAppSecret, body, r.Header.Get("X-Hub-Signature-256")); err != nil {
		code, status := "bad_signature", http.StatusUnauthorized
		if errors.Is(err, notify.ErrNoAppSecret) {
			code, status = "webhook_not_configured", http.StatusServiceUnavailable
		}
		a.log.WarnContext(r.Context(), "whatsapp webhook refused", "reason", code, "request_id", requestIDFrom(r.Context()))
		writeJSON(w, status, errorEnvelope{Error: map[string]any{
			"code": code, "message": "Webhook refused.", "request_id": requestIDFrom(r.Context()),
		}})
		return
	}
	ev, err := notify.ParseWebhook(body)
	if err != nil {
		a.fail(w, r, &badRequest{"The webhook body is not valid JSON."})
		return
	}
	res, err := a.reg.HandleWhatsAppEvent(r.Context(), ev)
	if err != nil {
		// A 500 makes the provider deliver again later; replies and receipts
		// are applied at most once, so that is safe.
		a.fail(w, r, err)
		return
	}
	a.log.InfoContext(r.Context(), "whatsapp webhook", "receipts", res.Receipts, "replies", res.Replies,
		"doses_answered", res.DosesAnswered, "opt_outs", res.OptOuts, "ignored", res.Ignored)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
