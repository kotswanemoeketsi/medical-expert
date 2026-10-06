package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/repository"
	"github.com/yourorg/medical-expert-system/internal/tenant"
)

// doseHorizon is how far ahead the scheduler keeps doses: far enough for a
// patient's app to show the week ahead, near enough that a change to a
// regimen or to the patient's hours only ever has a week of doses to rework.
const doseHorizon = 7 * 24 * time.Hour

var locations sync.Map // time zone name -> *time.Location

// practiceLocation is the practice's time zone. Dose times are wall-clock
// times where the patient lives, which for a South African practice is
// South Africa.
func practiceLocation(name string) *time.Location {
	if name == "" {
		return johannesburg
	}
	if v, ok := locations.Load(name); ok {
		return v.(*time.Location)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		loc = johannesburg
	}
	locations.Store(name, loc)
	return loc
}

// messagingOn reports whether a practice can message its patients: the
// platform has a sender, the practice has switched messaging on, and it is
// not a demo practice, whose patients' numbers are made up.
func (r *Registry) messagingOn(t *model.Tenant) bool {
	return r.sender != nil && t.Settings.WhatsAppEnabled && (!t.Settings.Demo || r.allowDemo)
}

// reachable reports whether a patient can be asked "did you take it?" at
// all: through their own login, or by WhatsApp with their consent.
//
// Doses are only scheduled for patients who can answer. A dose nobody can
// answer is closed as missed when its window ends, so scheduling doses for
// a patient who was never asked would manufacture non-adherence.
func reachable(c repository.ScheduleCandidate, messaging bool) bool {
	if !c.PatientActive {
		return false
	}
	if c.HasPortal {
		return true
	}
	return messaging && c.Channel == model.ChannelWhatsApp && c.HasMobile && c.ReminderConsent
}

// SyncResult counts what a scheduler run changed.
type SyncResult struct {
	Added, Removed int
}

func (s *SyncResult) add(o SyncResult) { s.Added += o.Added; s.Removed += o.Removed }

// syncSchedule brings one regimen's future scheduled doses in line with its
// frequency and the patient's waking hours, from now to the horizon. Doses
// already reminded or answered, and doses added by hand, are never touched.
func (b *base) syncSchedule(ctx context.Context, c repository.ScheduleCandidate, messaging bool, loc *time.Location, now time.Time) (SyncResult, error) {
	var res SyncResult
	repo := b.regimens()
	var want []time.Time
	if reachable(c, messaging) {
		want = c.Regimen.DoseTimesBetween(now, now.Add(doseHorizon), c.Waking, loc)
	}
	have, err := repo.FutureScheduledDoses(ctx, c.Regimen.ID, now)
	if err != nil {
		return res, err
	}
	wanted := make(map[int64]bool, len(want))
	for _, t := range want {
		wanted[t.UnixMilli()] = true
	}
	present := make(map[int64]bool, len(have))
	for _, d := range have {
		present[d.ScheduledAt.UnixMilli()] = true
		if wanted[d.ScheduledAt.UnixMilli()] {
			continue
		}
		gone, err := repo.DeleteScheduledDose(ctx, d.ID)
		if err != nil {
			return res, err
		}
		if gone {
			res.Removed++
		}
	}
	var missing []time.Time
	for _, t := range want {
		if !present[t.UnixMilli()] {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		n, err := repo.AddScheduledDoses(ctx, &c.Regimen, missing, now)
		if err != nil {
			return res, err
		}
		res.Added += n
	}
	return res, nil
}

// syncPractice runs the scheduler over every active regimen in one practice,
// and withdraws future doses of regimens that are no longer active.
func (b *base) syncPractice(ctx context.Context, t *model.Tenant) (SyncResult, error) {
	var total SyncResult
	loc := practiceLocation(t.Timezone)
	now := b.now()
	cands, err := b.regimens().ScheduleCandidates(ctx, model.DateOf(now.In(loc)))
	if err != nil {
		return total, err
	}
	messaging := b.reg.messagingOn(t)
	for _, c := range cands {
		res, err := b.syncSchedule(ctx, c, messaging, loc, now)
		if err != nil {
			return total, fmt.Errorf("regimen %s: %w", c.Regimen.ID, err)
		}
		total.add(res)
	}
	withdrawn, err := b.regimens().WithdrawInactiveDoses(ctx, now)
	total.Removed += int(withdrawn)
	return total, err
}

// refreshSchedule re-runs the scheduler for one patient straight after
// something it depends on changed: a new regimen, their waking hours, their
// consent. The hourly run would catch up anyway, so a failure here is
// logged rather than failing the change that triggered it.
func (b *base) refreshSchedule(ctx context.Context, patientID model.ID) {
	if err := b.syncPatient(ctx, patientID); err != nil {
		b.reg.log.WarnContext(ctx, "dose schedule refresh failed; the hourly run will retry",
			"patient_id", patientID.String(), "err", err)
	}
}

func (b *base) syncPatient(ctx context.Context, patientID model.ID) error {
	t, err := repository.NewPracticeRepository(b.scope).Get(ctx)
	if err != nil {
		return err
	}
	loc := practiceLocation(t.Timezone)
	now := b.now()
	cands, err := b.regimens().PatientScheduleCandidates(ctx, patientID, model.DateOf(now.In(loc)))
	if err != nil {
		return err
	}
	messaging := b.reg.messagingOn(t)
	for _, c := range cands {
		if _, err := b.syncSchedule(ctx, c, messaging, loc, now); err != nil {
			return err
		}
	}
	return nil
}

// forEachPractice runs fn once per active practice, each with its own scope
// and no user attached: these are the system's own jobs. Per practice
// rather than one global statement, so every statement stays tenant-scoped
// and one practice's failure does not stop the others.
func (r *Registry) forEachPractice(ctx context.Context, fn func(b *base, t *model.Tenant) error) error {
	tenants, err := r.platform.ActiveTenants(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for i := range tenants {
		t := &tenants[i]
		if !r.only.IsZero() && t.ID != r.only {
			continue
		}
		scope, err := tenant.NewScope(r.db, t.ID)
		if err != nil {
			return err
		}
		if err := fn(&base{reg: r, scope: scope, state: StateActive}, t); err != nil {
			r.log.ErrorContext(ctx, "practice job failed", "tenant", t.Slug, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("practice %s: %w", t.Slug, err)
			}
		}
	}
	return firstErr
}

// GenerateDoses is the hourly scheduler run (BR-4.2).
func (r *Registry) GenerateDoses(ctx context.Context) (SyncResult, error) {
	var total SyncResult
	err := r.forEachPractice(ctx, func(b *base, t *model.Tenant) error {
		res, err := b.syncPractice(ctx, t)
		total.add(res)
		return err
	})
	return total, err
}

// CloseOverdueDoses marks every dose whose answer window has closed as
// missed, in every practice.
func (r *Registry) CloseOverdueDoses(ctx context.Context) (int64, error) {
	var total int64
	err := r.forEachPractice(ctx, func(b *base, _ *model.Tenant) error {
		n, err := b.closeOverdue(ctx)
		total += n
		return err
	})
	return total, err
}

func (b *base) closeOverdue(ctx context.Context) (int64, error) {
	n, err := b.regimens().CloseOverdue(ctx, b.now().Add(-b.reg.cfg.DoseGrace))
	if err != nil {
		return n, err
	}
	if n > 0 {
		b.recordWrite(ctx, model.AuditEntry{Action: model.ActionDosesMissed, Outcome: model.OutcomeSuccess,
			Metadata: map[string]any{"count": n, "grace_hours": b.reg.cfg.DoseGrace.Hours()}})
	}
	return n, nil
}
