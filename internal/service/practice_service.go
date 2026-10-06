package service

import (
	"context"
	"errors"
	"net/mail"
	"strings"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/notify"
	"github.com/yourorg/medical-expert-system/internal/phone"
	"github.com/yourorg/medical-expert-system/internal/repository"
)

// PracticeService shows and changes the practice's own details and
// settings (BR-0.6).
type PracticeService struct{ *base }

// MessagingStatus says whether the practice can message patients, and if
// not, why not.
type MessagingStatus struct {
	// Configured: the server has a WhatsApp account.
	Configured bool `json:"configured"`
	// Enabled: the practice has switched messaging on.
	Enabled bool `json:"enabled"`
	// Active: both, and not a demo practice. Reminders go out.
	Active     bool   `json:"active"`
	SenderName string `json:"sender_name"`
	// FollowUpMinutes is the delay actually in force.
	FollowUpMinutes int  `json:"follow_up_minutes"`
	Demo            bool `json:"demo,omitempty"`
}

// PracticeView is the practice and its messaging status.
type PracticeView struct {
	Practice  *model.Tenant   `json:"practice"`
	Messaging MessagingStatus `json:"messaging"`
}

func (s *PracticeService) view(t *model.Tenant) *PracticeView {
	return &PracticeView{Practice: t, Messaging: MessagingStatus{
		Configured: s.reg.sender != nil, Enabled: t.Settings.WhatsAppEnabled, Active: s.reg.messagingOn(t),
		SenderName: t.Settings.SenderName(t.Name), FollowUpMinutes: int(s.reg.followUpAfter(t).Minutes()),
		Demo: t.Settings.Demo,
	}}
}

// Get returns the practice's details. Any staff member may read them.
func (s *PracticeService) Get(ctx context.Context) (*PracticeView, error) {
	if err := s.requireStaff(); err != nil {
		return nil, err
	}
	t, err := repository.NewPracticeRepository(s.scope).Get(ctx)
	if err != nil {
		return nil, err
	}
	return s.view(t), nil
}

// PracticeUpdate changes what is given; nil leaves a field alone.
type PracticeUpdate struct {
	Name            *string
	PracticeNumber  *string
	Address         *string
	Phone           *string
	Email           *string
	WhatsAppEnabled *bool
	MessagingName   *string
	// FollowUpMinutes: a number of minutes, 0 for none, or -1 to go back to
	// the platform default.
	FollowUpMinutes *int
}

// Update changes the practice's details and settings. Administrators only.
// Switching messaging on is refused while the name patients would see says
// what the practice treats.
func (s *PracticeService) Update(ctx context.Context, in PracticeUpdate) (*PracticeView, error) {
	if err := s.requireAdmin(); err != nil {
		return nil, err
	}
	repo := repository.NewPracticeRepository(s.scope)
	t, err := repo.Get(ctx)
	if err != nil {
		return nil, err
	}
	before := *t
	var v model.ValidationErrors
	var changed []string
	set := func(field string, dst *string, val *string, max int, required bool) {
		if val == nil {
			return
		}
		x := collapseSpaces(*val)
		switch {
		case required && x == "":
			v.Add(field, "is required")
			return
		case len(x) > max:
			v.Add(field, "must be at most %d characters", max)
			return
		}
		if x != *dst {
			*dst, changed = x, append(changed, field)
		}
	}
	set("name", &t.Name, in.Name, 255, true)
	set("practice_number", &t.PracticeNumber, in.PracticeNumber, 32, false)
	set("address", &t.Address, in.Address, 500, false)
	if in.Phone != nil {
		x := strings.TrimSpace(*in.Phone)
		if x != "" {
			norm, err := phone.NormalizeE164(x)
			if err != nil {
				v.Add("phone", "is not a valid phone number")
			}
			x = norm
		}
		if x != t.Phone {
			t.Phone, changed = x, append(changed, "phone")
		}
	}
	if in.Email != nil {
		x := strings.ToLower(strings.TrimSpace(*in.Email))
		if x != "" {
			if a, err := mail.ParseAddress(x); err != nil || a.Address != x || len(x) > 255 {
				v.Add("email", "is not a valid email address")
			}
		}
		if x != t.Email {
			t.Email, changed = x, append(changed, "email")
		}
	}
	set("messaging_name", &t.Settings.MessagingName, in.MessagingName, 60, false)
	if in.FollowUpMinutes != nil {
		m := *in.FollowUpMinutes
		switch {
		case m == -1:
			if t.Settings.FollowUpMinutes != nil {
				t.Settings.FollowUpMinutes, changed = nil, append(changed, "follow_up_minutes")
			}
		case m < 0 || m > 720:
			v.Add("follow_up_minutes", "must be between 0 (no follow-up) and 720, or -1 for the default")
		default:
			if t.Settings.FollowUpMinutes == nil || *t.Settings.FollowUpMinutes != m {
				t.Settings.FollowUpMinutes, changed = &m, append(changed, "follow_up_minutes")
			}
		}
	}
	if in.WhatsAppEnabled != nil && *in.WhatsAppEnabled != t.Settings.WhatsAppEnabled {
		if *in.WhatsAppEnabled && t.Settings.Demo {
			return nil, ErrDemoPractice
		}
		t.Settings.WhatsAppEnabled, changed = *in.WhatsAppEnabled, append(changed, "whatsapp_enabled")
	}
	if len(v) > 0 {
		return nil, invalid(v)
	}
	if t.Settings.WhatsAppEnabled {
		terms, err := s.reg.medicationTerms(ctx)
		if err != nil {
			return nil, err
		}
		if err := notify.CheckNoClinicalDetail([]string{t.Settings.SenderName(t.Name)}, terms); err != nil {
			var ce *notify.ClinicalDetailError
			if errors.As(err, &ce) {
				return nil, &Error{Kind: KindInvalid, Code: "clinical_detail",
					Message: "Patients would see this practice named as " + quote(t.Settings.SenderName(t.Name)) +
						", which could reveal what they are treated for. Set a neutral messaging_name.",
					Fields: []model.FieldError{{Field: "messaging_name", Message: "is needed: " + ce.Error()}}}
			}
			return nil, err
		}
	}
	if len(changed) == 0 {
		return s.view(t), nil
	}
	if err := repo.Update(ctx, t, s.now()); err != nil {
		return nil, err
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionPracticeUpdated, Outcome: model.OutcomeSuccess,
		SubjectType: "practice", SubjectID: idp(t.ID), Metadata: map[string]any{"fields": changed}})
	if before.Settings.WhatsAppEnabled != t.Settings.WhatsAppEnabled {
		// Who can be reminded has just changed for every patient: bring the
		// schedule in line now rather than at the next hourly run.
		if _, err := s.syncPractice(ctx, t); err != nil {
			s.reg.log.WarnContext(ctx, "dose schedule refresh after a settings change failed; the hourly run will retry", "err", err)
		}
	}
	return s.view(t), nil
}

// ConfigureDemoPractice marks a practice as a demo, whose patients are made
// up, and switches its messaging on or off. Only the demo seeder calls it:
// it is not reachable over HTTP, and a demo practice cannot be turned back
// into a real one. Messaging on a demo practice only ever reaches a
// stand-in sender (Deps.AllowDemoSends).
func (r *Registry) ConfigureDemoPractice(ctx context.Context, tenantID model.ID, messaging bool) error {
	b, err := r.systemBase(tenantID)
	if err != nil {
		return err
	}
	repo := repository.NewPracticeRepository(b.scope)
	t, err := repo.Get(ctx)
	if err != nil {
		return err
	}
	t.Settings.Demo, t.Settings.WhatsAppEnabled = true, messaging
	if err := repo.Update(ctx, t, r.now()); err != nil {
		return err
	}
	_, err = b.syncPractice(ctx, t)
	return err
}

func quote(s string) string { return "“" + s + "”" }
