package service

import (
	"testing"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/repository"
)

func TestOnlyPatientsWhoCanAnswerAreReachable(t *testing.T) {
	onWhatsApp := repository.ScheduleCandidate{PatientActive: true, Channel: model.ChannelWhatsApp, HasMobile: true, ReminderConsent: true}
	cases := []struct {
		name      string
		c         repository.ScheduleCandidate
		messaging bool
		want      bool
	}{
		{"on WhatsApp with consent", onWhatsApp, true, true},
		{"practice messaging off", onWhatsApp, false, false},
		{"no consent", repository.ScheduleCandidate{PatientActive: true, Channel: model.ChannelWhatsApp, HasMobile: true}, true, false},
		{"no number", repository.ScheduleCandidate{PatientActive: true, Channel: model.ChannelWhatsApp, ReminderConsent: true}, true, false},
		{"prefers SMS", repository.ScheduleCandidate{PatientActive: true, Channel: model.ChannelSMS, HasMobile: true, ReminderConsent: true}, true, false},
		{"own login, messaging off", repository.ScheduleCandidate{PatientActive: true, HasPortal: true}, false, true},
		{"inactive patient with a login", repository.ScheduleCandidate{HasPortal: true}, true, false},
	}
	for _, tc := range cases {
		if got := reachable(tc.c, tc.messaging); got != tc.want {
			t.Errorf("%s: reachable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestOneMessageCoversAPatientsDosesAtOneMoment(t *testing.T) {
	p1, p2 := model.MustNewID(), model.MustNewID()
	eight := time.Date(2026, 10, 5, 6, 0, 0, 0, time.UTC)
	due := []repository.DueDose{
		{DoseID: model.MustNewID(), PatientID: p1, ScheduledAt: eight},
		{DoseID: model.MustNewID(), PatientID: p1, ScheduledAt: eight},
		{DoseID: model.MustNewID(), PatientID: p1, ScheduledAt: eight.Add(5 * time.Minute)},
		{DoseID: model.MustNewID(), PatientID: p2, ScheduledAt: eight},
	}
	groups := groupDoses(due)
	if len(groups) != 3 || len(groups[0].doseIDs) != 2 || len(groups[1].doseIDs) != 1 || groups[2].patientID != p2 {
		t.Fatalf("groups: %+v", groups)
	}
	if backoff(1) != time.Minute || backoff(2) != 5*time.Minute {
		t.Fatal("back-off changed")
	}
}
