package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yourorg/medical-expert-system/internal/ai"
	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/repository"
)

// EncounterService is the consultation note (BR-2.4, BR-2.7) and AI
// drafting of it (M5, narrowed to transcription).
type EncounterService struct{ *base }

const maxNoteField = 20000

// EncounterInput is the content of a draft note.
type EncounterInput struct {
	Type                model.EncounterType
	EncounterAt         *time.Time // empty: now
	PresentingComplaint string
	History             string
	Examination         string
	Assessment          string
	Plan                string
	Vitals              *model.Vitals
}

func (in *EncounterInput) validate(now time.Time) model.ValidationErrors {
	var v model.ValidationErrors
	if in.Type == "" {
		in.Type = model.EncounterConsultation
	}
	if !in.Type.Valid() {
		v.Add("encounter_type", "must be consultation, follow_up, telephonic, home_visit, procedure or other")
	}
	if in.EncounterAt == nil || in.EncounterAt.IsZero() {
		at := now
		in.EncounterAt = &at
	}
	at := in.EncounterAt.UTC().Truncate(time.Millisecond)
	in.EncounterAt = &at
	switch {
	case at.After(now.Add(5 * time.Minute)):
		v.Add("encounter_at", "cannot be in the future")
	case at.Before(now.AddDate(-5, 0, 0)):
		v.Add("encounter_at", "is more than five years ago")
	}
	for _, f := range []struct {
		name string
		val  *string
	}{
		{"presenting_complaint", &in.PresentingComplaint}, {"history", &in.History}, {"examination", &in.Examination},
		{"assessment", &in.Assessment}, {"plan", &in.Plan},
	} {
		*f.val = strings.TrimSpace(*f.val)
		if len(*f.val) > maxNoteField {
			v.Add(f.name, "must be at most %d characters", maxNoteField)
		}
	}
	v = append(v, in.Vitals.Validate()...)
	if in.Vitals.IsEmpty() {
		in.Vitals = nil
	}
	return v
}

func (in *EncounterInput) apply(e *model.Encounter) {
	e.Type, e.EncounterAt = in.Type, *in.EncounterAt
	e.PresentingComplaint, e.History, e.Examination = in.PresentingComplaint, in.History, in.Examination
	e.Assessment, e.Plan, e.Vitals = in.Assessment, in.Plan, in.Vitals
}

// Create starts a draft note, authored by the actor.
func (s *EncounterService) Create(ctx context.Context, patientID model.ID, in EncounterInput) (*model.Encounter, error) {
	if err := s.requireClinical(); err != nil {
		return nil, err
	}
	now := s.now()
	if v := in.validate(now); len(v) > 0 {
		return nil, invalid(v)
	}
	if err := s.requirePatientExists(ctx, patientID); err != nil {
		return nil, err
	}
	e := &model.Encounter{ID: model.MustNewID(), PatientID: patientID, ClinicianID: s.actor.UserID,
		Status: model.EncounterDraft, CreatedAt: now, UpdatedAt: now}
	in.apply(e)
	if err := s.encounters().Create(ctx, e); err != nil {
		return nil, fromRepo(err)
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionEncounterCreated, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(patientID), Metadata: map[string]any{"encounter_id": e.ID.String()}})
	return s.encounters().Get(ctx, e.ID)
}

// EncounterView is a note with its amendment history.
type EncounterView struct {
	Encounter  *model.Encounter
	Amendments []model.Amendment
}

// Get returns a note and its amendments.
func (s *EncounterService) Get(ctx context.Context, id model.ID) (*EncounterView, error) {
	if err := s.requireClinical(); err != nil {
		return nil, err
	}
	repo := s.encounters()
	e, err := repo.Get(ctx, id)
	if err != nil {
		return nil, fromRepo(err)
	}
	amendments, err := repo.Amendments(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.readAudited(ctx, model.ActionEncounterRead, "patient", e.PatientID,
		map[string]any{"encounter_id": id.String()}); err != nil {
		return nil, err
	}
	return &EncounterView{Encounter: e, Amendments: amendments}, nil
}

// List returns a patient's notes, newest first. A patient sees only signed
// notes: an unfinished draft is not yet part of their record.
func (s *EncounterService) List(ctx context.Context, patientID model.ID) ([]model.Encounter, error) {
	if err := s.canSeePatientClinical(ctx, patientID); err != nil {
		return nil, err
	}
	if err := s.requirePatientExists(ctx, patientID); err != nil {
		return nil, err
	}
	list, err := s.encounters().ForPatient(ctx, patientID, !s.actor.Role.IsClinical(), 100)
	if err != nil {
		return nil, err
	}
	if err := s.readAudited(ctx, model.ActionEncountersListed, "patient", patientID, map[string]any{"count": len(list)}); err != nil {
		return nil, err
	}
	return list, nil
}

// editable loads a note the actor may change: their own draft.
func (s *EncounterService) editable(ctx context.Context, id model.ID) (*model.Encounter, error) {
	if err := s.requireClinical(); err != nil {
		return nil, err
	}
	e, err := s.encounters().Get(ctx, id)
	if err != nil {
		return nil, fromRepo(err)
	}
	if e.ClinicianID != s.actor.UserID {
		return nil, ErrNotAuthor
	}
	if e.Status != model.EncounterDraft {
		return nil, ErrNoteSigned
	}
	return e, nil
}

// Update replaces a draft's text and vitals: the editor sends the whole
// note. A request that leaves out the type or the time keeps the stored
// ones; resetting them to "consultation, now" would quietly move a note.
// Only its author may update it.
func (s *EncounterService) Update(ctx context.Context, id model.ID, in EncounterInput) (*model.Encounter, error) {
	e, err := s.editable(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Type == "" {
		in.Type = e.Type
	}
	if in.EncounterAt == nil || in.EncounterAt.IsZero() {
		at := e.EncounterAt
		in.EncounterAt = &at
	}
	now := s.now()
	if v := in.validate(now); len(v) > 0 {
		return nil, invalid(v)
	}
	in.apply(e)
	e.UpdatedAt = now
	if err := s.encounters().UpdateDraft(ctx, e, s.actor.UserID); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, ErrNoteSigned
		}
		return nil, err
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionEncounterUpdated, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(e.PatientID), Metadata: map[string]any{"encounter_id": id.String()}})
	return s.encounters().Get(ctx, id)
}

// Sign makes a note part of the record. Only its author may sign it, and
// only once it records what was decided: an entry saying someone attended
// without saying what was concluded is worse than none.
func (s *EncounterService) Sign(ctx context.Context, id model.ID) (*model.Encounter, error) {
	e, err := s.editable(ctx, id)
	if err != nil {
		return nil, err
	}
	if e.Assessment == "" && e.Plan == "" {
		return nil, &Error{Kind: KindInvalid, Code: "validation_failed",
			Message: "A note needs an assessment or a plan before it can be signed.",
			Fields:  []model.FieldError{{Field: "assessment", Message: "or a plan is required before signing"}}}
	}
	if err := s.encounters().Sign(ctx, id, s.actor.UserID, s.now()); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, ErrNoteSigned
		}
		return nil, err
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionEncounterSigned, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(e.PatientID),
		Metadata: map[string]any{"encounter_id": id.String(), "ai_assisted": e.AIAssisted}})
	return s.encounters().Get(ctx, id)
}

// AmendmentInput corrects one field of a signed note.
type AmendmentInput struct {
	Field    model.NoteField
	NewValue string
	Reason   string
}

// Amend corrects a signed note. The previous text is kept, with who changed
// it, when and why; the original signature stands.
func (s *EncounterService) Amend(ctx context.Context, id model.ID, in AmendmentInput) (*EncounterView, error) {
	if err := s.requireClinical(); err != nil {
		return nil, err
	}
	in.NewValue, in.Reason = strings.TrimSpace(in.NewValue), strings.TrimSpace(in.Reason)
	var v model.ValidationErrors
	if !in.Field.Valid() {
		v.Add("field", "must be presenting_complaint, history, examination, assessment or plan; who saw the patient and when cannot be amended")
	}
	if len(in.NewValue) > maxNoteField {
		v.Add("new_value", "must be at most %d characters", maxNoteField)
	}
	if in.Reason == "" || len(in.Reason) > 500 {
		v.Add("reason", "is required (at most 500 characters): say why the note is being corrected")
	}
	if len(v) > 0 {
		return nil, invalid(v)
	}
	repo := s.encounters()
	e, err := repo.Get(ctx, id)
	if err != nil {
		return nil, fromRepo(err)
	}
	if e.Status != model.EncounterSigned {
		return nil, ErrNoteNotSigned
	}
	if e.Field(in.Field) == in.NewValue {
		return nil, invalidField("new_value", "is the same as the current text")
	}
	now := s.now()
	a := &model.Amendment{ID: model.MustNewID(), EncounterID: id, Field: in.Field, NewValue: in.NewValue,
		Reason: in.Reason, AmendedBy: s.actor.UserID, AmendedAt: now}
	if err := repo.Amend(ctx, a); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, ErrNoteNotSigned
		}
		return nil, fromRepo(err)
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionEncounterAmended, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(e.PatientID), Reason: in.Reason,
		Metadata: map[string]any{"encounter_id": id.String(), "field": string(in.Field), "amendment_id": a.ID.String()}})
	return s.Get(ctx, id)
}

// ---------------------------------------------------------------------------
// AI drafting
// ---------------------------------------------------------------------------

const maxDictation = 20000

// draftBudget bounds a drafting request, retries included.
const draftBudget = 120 * time.Second

// DraftNote asks the model to organise a dictation into the five note
// fields. The result is stored beside the note and returned to the
// clinician; it never touches the note itself. The clinician applies what
// they want and signs.
//
// Nothing is sent without the patient's consent to AI-assisted review, and
// the check happens before the model is called, not after.
func (s *EncounterService) DraftNote(ctx context.Context, id model.ID, dictation string) (*model.NoteDraft, error) {
	e, err := s.editable(ctx, id)
	if err != nil {
		return nil, err
	}
	dictation = strings.TrimSpace(dictation)
	if dictation == "" || utf8.RuneCountInString(dictation) > maxDictation {
		return nil, invalidField("dictation", fmt.Sprintf("is required (at most %d characters)", maxDictation))
	}
	if s.reg.drafter == nil {
		return nil, ErrAIUnavailable
	}
	patient, err := s.patients().Get(ctx, e.PatientID)
	if err != nil {
		return nil, fromRepo(err)
	}
	consent, err := s.patients().ConsentState(ctx, e.PatientID)
	if err != nil {
		return nil, err
	}
	if !consent.Granted(model.ConsentAIAssistedReview) {
		s.recordWrite(ctx, model.AuditEntry{Action: model.ActionAIDraftRefused, Outcome: model.OutcomeDenied,
			SubjectType: "patient", SubjectID: idp(e.PatientID), Reason: "no ai_assisted_review consent",
			Metadata: map[string]any{"encounter_id": id.String()}})
		return nil, ErrAIConsentRequired
	}
	repo := s.encounters()
	used, err := repo.DraftsSince(ctx, s.startOfToday())
	if err != nil {
		return nil, err
	}
	if used >= s.reg.cfg.AIDailyLimit {
		return nil, ErrAILimitReached
	}

	background, err := s.aiContext(ctx, patient)
	if err != nil {
		return nil, err
	}
	sent, redactions := redactIdentifiers(dictation, patient)
	contextJSON, _ := json.Marshal(background)

	now := s.now()
	draft := &model.NoteDraft{ID: model.MustNewID(), EncounterID: id, Dictation: dictation,
		Model: s.reg.drafter.Model(), CreatedBy: s.actor.UserID, CreatedAt: now}

	// One overall budget, retries included, so a slow provider cannot hold
	// a clinician's request open indefinitely.
	callCtx, cancel := context.WithTimeout(ctx, draftBudget)
	res, callErr := s.reg.drafter.DraftNote(callCtx, ai.Request{Dictation: sent, Context: background})
	cancel()
	if callErr != nil {
		draft.Status, draft.Error = model.DraftFailed, callErr.Error()
		if err := repo.SaveDraft(ctx, draft, contextJSON); err != nil {
			s.reg.log.ErrorContext(ctx, "could not store failed draft", "err", err)
		}
		s.reg.log.WarnContext(ctx, "note drafting failed", "err", callErr)
		s.recordWrite(ctx, model.AuditEntry{Action: model.ActionAIDraftFailed, Outcome: model.OutcomeFailure,
			SubjectType: "patient", SubjectID: idp(e.PatientID), Reason: truncateText(callErr.Error(), 255),
			Metadata: map[string]any{"encounter_id": id.String(), "draft_id": draft.ID.String()}})
		return nil, ErrAIFailed
	}

	content := &model.DraftContent{
		PresentingComplaint: res.Draft.PresentingComplaint, History: res.Draft.History,
		Examination: res.Draft.Examination, Assessment: res.Draft.Assessment, Plan: res.Draft.Plan,
		Flags: append([]string{}, res.Draft.Flags...),
	}
	if len(res.Discarded) > 0 {
		content.Flags = append(content.Flags, fmt.Sprintf(
			"The drafting service returned content outside the note (%s). It was discarded unread.",
			strings.Join(res.Discarded, ", ")))
	}
	if redactions > 0 {
		content.Flags = append(content.Flags,
			"The patient's name or ID number was replaced with a placeholder before drafting; check the wording.")
	}
	draft.Draft, draft.Status = content, model.DraftGenerated
	if res.Model != "" {
		draft.Model = res.Model
	}
	draft.InputTokens, draft.OutputTokens = res.InputTokens, res.OutputTokens
	if err := repo.SaveDraft(ctx, draft, contextJSON); err != nil {
		return nil, err
	}
	// Provenance, permanent: a model drafted text for this note.
	if err := repo.MarkAIAssisted(ctx, id); err != nil {
		return nil, err
	}
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionAIDraftGenerated, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(e.PatientID),
		Metadata: map[string]any{"encounter_id": id.String(), "draft_id": draft.ID.String(), "model": draft.Model,
			"input_tokens": res.InputTokens, "output_tokens": res.OutputTokens, "discarded": res.Discarded}})
	return draft, nil
}

// Drafts lists the AI drafts made for a note, newest first.
func (s *EncounterService) Drafts(ctx context.Context, id model.ID) ([]model.NoteDraft, error) {
	if err := s.requireClinical(); err != nil {
		return nil, err
	}
	e, err := s.encounters().Get(ctx, id)
	if err != nil {
		return nil, fromRepo(err)
	}
	list, err := s.encounters().Drafts(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.readAudited(ctx, model.ActionAIDraftsRead, "patient", e.PatientID,
		map[string]any{"encounter_id": id.String()}); err != nil {
		return nil, err
	}
	return list, nil
}

// FlagDraft marks a draft as incorrect or unsafe, for the review the
// business committed to (BR-5.15).
func (s *EncounterService) FlagDraft(ctx context.Context, encounterID, draftID model.ID, reason string) error {
	if err := s.requireClinical(); err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 1000 {
		return invalidField("reason", "is required (at most 1000 characters): say what was wrong")
	}
	e, err := s.encounters().Get(ctx, encounterID)
	if err != nil {
		return fromRepo(err)
	}
	if err := s.encounters().FlagDraft(ctx, encounterID, draftID, s.actor.UserID, reason, s.now()); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			drafts, derr := s.encounters().Drafts(ctx, encounterID)
			if derr == nil {
				for _, d := range drafts {
					if d.ID == draftID {
						return ErrDraftAlreadyFlagged
					}
				}
			}
			return ErrNotFound
		}
		return err
	}
	s.reg.log.WarnContext(ctx, "AI draft flagged for review", "tenant", s.scope.TenantID().String(),
		"draft", draftID.String())
	s.recordWrite(ctx, model.AuditEntry{Action: model.ActionAIDraftFlagged, Outcome: model.OutcomeSuccess,
		SubjectType: "patient", SubjectID: idp(e.PatientID), Reason: truncateText(reason, 255),
		Metadata: map[string]any{"encounter_id": encounterID.String(), "draft_id": draftID.String()}})
	return nil
}

// aiContext is the background sent with a dictation: enough to spell a
// mumbled drug name, not the whole file, and nothing that identifies the
// patient.
func (b *base) aiContext(ctx context.Context, p *model.Patient) (ai.PatientContext, error) {
	c := ai.PatientContext{AgeYears: p.AgeAt(b.now()), Sex: string(p.Sex),
		Allergies: []string{}, Conditions: []string{}, Medication: []string{}}
	allergies, err := b.patients().Allergies(ctx, p.ID)
	if err != nil {
		return c, err
	}
	for _, a := range allergies {
		if a.Status == model.ClinicalActive {
			c.Allergies = append(c.Allergies, fmt.Sprintf("%s (%s)", a.Substance, strings.ReplaceAll(string(a.Severity), "_", " ")))
		}
	}
	conditions, err := b.patients().Conditions(ctx, p.ID)
	if err != nil {
		return c, err
	}
	for _, cond := range conditions {
		if cond.Status == model.ClinicalActive {
			d := cond.Description
			if cond.ICD10Code != "" {
				d += " (" + cond.ICD10Code + ")"
			}
			c.Conditions = append(c.Conditions, d)
		}
	}
	regimens, err := b.regimens().Current(ctx, p.ID)
	if err != nil {
		return c, err
	}
	for _, r := range regimens {
		if r.Status == model.RegimenActive {
			c.Medication = append(c.Medication, r.Describe())
		}
	}
	return c, nil
}

// redactIdentifiers replaces the patient's names and ID number in a
// dictation before it leaves the building. Whole words only, so "Hope" the
// surname is replaced but "hopeful" is not. Returns how many replacements
// were made.
func redactIdentifiers(text string, p *model.Patient) (string, int) {
	count := 0
	replace := func(needle, with string) {
		needle = strings.TrimSpace(needle)
		if utf8.RuneCountInString(needle) < 3 {
			return
		}
		re, err := regexp.Compile(`(?i)` + regexp.QuoteMeta(needle))
		if err != nil {
			return
		}
		var b strings.Builder
		last := 0
		for _, m := range re.FindAllStringIndex(text, -1) {
			before, _ := utf8.DecodeLastRuneInString(text[:m[0]])
			after, _ := utf8.DecodeRuneInString(text[m[1]:])
			if isWordRune(before) || isWordRune(after) {
				continue
			}
			b.WriteString(text[last:m[0]])
			b.WriteString(with)
			last = m[1]
			count++
		}
		b.WriteString(text[last:])
		text = b.String()
	}
	for _, part := range strings.Fields(p.FirstName + " " + p.LastName) {
		replace(part, "[patient]")
	}
	if p.NationalID != "" {
		replace(p.NationalID, "[id number]")
	}
	return text, count
}

func isWordRune(r rune) bool {
	return r != utf8.RuneError && (unicode.IsLetter(r) || unicode.IsDigit(r))
}

// startOfToday is midnight in South Africa, for daily limits.
func (b *base) startOfToday() time.Time {
	d := b.today()
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, johannesburg)
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
