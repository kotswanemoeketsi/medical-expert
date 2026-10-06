//go:build integration

package repository

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/tenant"
)

func newDose(t *testing.T, scope *tenant.Scope, g *model.Regimen, at time.Time) model.ID {
	t.Helper()
	if _, err := NewRegimenRepository(scope).AddScheduledDoses(context.Background(), g, []time.Time{at}, now); err != nil {
		t.Fatal(err)
	}
	doses, err := NewRegimenRepository(scope).Doses(context.Background(), g.PatientID, at.Add(-time.Second), at.Add(time.Second))
	if err != nil || len(doses) == 0 {
		t.Fatalf("dose not found: %v", err)
	}
	return doses[0].ID
}

func reminderFor(patientID model.ID, doses ...model.ID) *model.Notification {
	lease := now.Add(2 * time.Minute)
	return &model.Notification{ID: model.MustNewID(), PatientID: patientID, Channel: model.ChannelWhatsApp,
		Purpose: model.PurposeDoseReminder, Destination: "+27820000001", TemplateName: "mes_dose_reminder",
		TemplateLanguage: "en", Params: []string{"A", "08:00"}, Preview: "Hi A", Status: model.NotificationQueued,
		Attempts: 1, NextAttemptAt: &lease, CreatedAt: now, UpdatedAt: now, DoseIDs: doses}
}

func TestReminderClaimsAreExclusive(t *testing.T) {
	conn := openTestDB(t)
	_, scope := newPractice(t, conn)
	doc := newUser(t, scope, model.RoleDoctor)
	p := newPatient(t, scope, "Claim", "Test", model.NewDate(1970, 1, 1))
	g := newRegimen(t, scope, conn, p.ID, doc.ID)
	dose := newDose(t, scope, g, now.Add(-time.Minute).Truncate(time.Second))
	repo := NewNotificationRepository(scope)

	var wg sync.WaitGroup
	var mu sync.Mutex
	won, lost := 0, 0
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := repo.Create(context.Background(), reminderFor(p.ID, dose), ClaimExclusive)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, ErrConflict):
				lost++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if won != 1 || lost != 3 {
		t.Fatalf("four dispatchers claimed one dose: %d won, %d lost", won, lost)
	}
	list, _ := repo.ForPatient(context.Background(), p.ID, 10)
	if len(list) != 1 || len(list[0].DoseIDs) != 1 {
		t.Fatalf("a lost claim left a notification behind: %+v", list)
	}

	// A reminder sent by hand does not need the dose unclaimed.
	if err := repo.Create(context.Background(), reminderFor(p.ID, dose), ClaimMark); err != nil {
		t.Fatalf("manual reminder: %v", err)
	}
}

func TestLeaseKeepsSendersApart(t *testing.T) {
	conn := openTestDB(t)
	_, scope := newPractice(t, conn)
	p := newPatient(t, scope, "Lease", "Test", model.NewDate(1970, 1, 1))
	repo := NewNotificationRepository(scope)
	n := reminderFor(p.ID)
	n.Purpose, n.TemplateName = model.PurposeRecall, "mes_recall"
	past := now.Add(-time.Minute)
	n.NextAttemptAt = &past
	if err := repo.Create(context.Background(), n, ClaimNone); err != nil {
		t.Fatal(err)
	}
	if err := repo.Lease(context.Background(), n.ID, now, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("first lease: %v", err)
	}
	if err := repo.Lease(context.Background(), n.ID, now, now.Add(2*time.Minute)); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second sender took a leased message: %v", err)
	}
	got, _ := repo.Get(context.Background(), n.ID)
	if got.Attempts != 2 {
		t.Fatalf("attempts not counted: %d", got.Attempts)
	}
	// Once the lease runs out, a retry may take it.
	if err := repo.Lease(context.Background(), n.ID, now.Add(3*time.Minute), now.Add(5*time.Minute)); err != nil {
		t.Fatalf("expired lease not reclaimable: %v", err)
	}
}

func TestReceiptsOnlyMoveForward(t *testing.T) {
	conn := openTestDB(t)
	_, scope := newPractice(t, conn)
	p := newPatient(t, scope, "Receipt", "Test", model.NewDate(1970, 1, 1))
	repo := NewNotificationRepository(scope)
	n := reminderFor(p.ID)
	if err := repo.Create(context.Background(), n, ClaimNone); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkSent(context.Background(), n.ID, "wamid.receipt-"+n.ID.String(), now); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		status  model.NotificationStatus
		changed bool
	}{
		{model.NotificationRead, true},
		{model.NotificationDelivered, false},
		{model.NotificationFailed, false},
		{model.NotificationSent, false},
	}
	for _, s := range steps {
		changed, err := repo.ApplyReceipt(context.Background(), n.ID, s.status, now, "131026", "x", now)
		if err != nil || changed != s.changed {
			t.Fatalf("receipt %s: changed=%v err=%v, want changed=%v", s.status, changed, err, s.changed)
		}
	}
	got, _ := repo.Get(context.Background(), n.ID)
	if got.Status != model.NotificationRead || got.DeliveredAt == nil || got.ErrorCode != "" {
		t.Fatalf("final state: %+v", got)
	}
}

func TestAReplyIsAppliedOnce(t *testing.T) {
	conn := openTestDB(t)
	_, scope := newPractice(t, conn)
	doc := newUser(t, scope, model.RoleDoctor)
	p := newPatient(t, scope, "Reply", "Test", model.NewDate(1970, 1, 1))
	g := newRegimen(t, scope, conn, p.ID, doc.ID)
	dose := newDose(t, scope, g, now.Add(-time.Minute).Truncate(time.Second))
	repo := NewNotificationRepository(scope)
	n := reminderFor(p.ID, dose)
	if err := repo.Create(context.Background(), n, ClaimExclusive); err != nil {
		t.Fatal(err)
	}
	providerID := "wamid.reply-" + n.ID.String()
	reply := func() *model.NotificationReply {
		return &model.NotificationReply{ID: model.MustNewID(), PatientID: p.ID, NotificationID: &n.ID, ProviderID: providerID,
			ReceivedAt: now, Interpretation: model.ReplyTaken, Body: "TAKEN", CreatedAt: now}
	}
	out, err := repo.RecordReply(context.Background(), reply(), []model.ID{dose}, "")
	if err != nil || out.Duplicate || len(out.DosesUpdated) != 1 {
		t.Fatalf("first delivery: %+v %v", out, err)
	}
	out, err = repo.RecordReply(context.Background(), reply(), []model.ID{dose}, "")
	if err != nil || !out.Duplicate || len(out.DosesUpdated) != 0 {
		t.Fatalf("second delivery: %+v %v", out, err)
	}
	d, _ := NewRegimenRepository(scope).Dose(context.Background(), dose)
	if d.Status != model.DoseTaken || d.ResponseChannel != model.ResponseWhatsApp || d.RespondedBy != nil {
		t.Fatalf("dose: %+v", d)
	}
	replies, _ := repo.RepliesForPatient(context.Background(), p.ID, 10)
	if len(replies) != 1 || replies[0].DosesUpdated != 1 {
		t.Fatalf("replies: %+v", replies)
	}
}

func TestSchedulerOnlyWithdrawsItsOwnUntouchedDoses(t *testing.T) {
	conn := openTestDB(t)
	_, scope := newPractice(t, conn)
	doc := newUser(t, scope, model.RoleDoctor)
	p := newPatient(t, scope, "Own", "Doses", model.NewDate(1970, 1, 1))
	g := newRegimen(t, scope, conn, p.ID, doc.ID)
	regs := NewRegimenRepository(scope)
	ctx := context.Background()
	future := now.Add(24 * time.Hour).Truncate(time.Second)

	manual := future.Add(time.Hour)
	if _, err := regs.ScheduleDoses(ctx, g, []time.Time{manual}, now); err != nil {
		t.Fatal(err)
	}
	scheduled := newDose(t, scope, g, future)
	reminded := newDose(t, scope, g, future.Add(2*time.Hour))
	if err := NewNotificationRepository(scope).Create(ctx, reminderFor(p.ID, reminded), ClaimExclusive); err != nil {
		t.Fatal(err)
	}
	all, _ := regs.Doses(ctx, p.ID, now, future.Add(3*time.Hour))
	var manualID model.ID
	for _, d := range all {
		if d.Source == model.DoseManual {
			manualID = d.ID
		}
	}
	for id, want := range map[model.ID]bool{manualID: false, reminded: false, scheduled: true} {
		gone, err := regs.DeleteScheduledDose(ctx, id)
		if err != nil || gone != want {
			t.Fatalf("delete %s: gone=%v err=%v, want %v", id, gone, err, want)
		}
	}
	owned, _ := regs.FutureScheduledDoses(ctx, g.ID, now)
	if len(owned) != 0 {
		t.Fatalf("a reminded dose is listed as the scheduler's to change: %+v", owned)
	}
}
