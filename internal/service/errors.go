package service

import (
	"errors"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/repository"
)

// Kind classifies an error by what the caller can do about it. The HTTP
// layer maps kinds to status codes in exactly one place; services never know
// status codes exist.
type Kind int

const (
	KindInvalid         Kind = iota + 1 // the input is wrong; fix it and retry
	KindUnauthenticated                 // not signed in, or the proof offered is wrong
	KindForbidden                       // signed in, but not allowed
	KindNotFound                        // does not exist, or not visible to this caller
	KindConflict                        // the record is not in a state that allows this
	KindUnavailable                     // a dependency is switched off or unreachable
)

// Error is the one error type services return to callers.
type Error struct {
	Kind    Kind
	Code    string // stable, machine-readable
	Message string // for a person
	Fields  []model.FieldError
	Details map[string]any
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Shared errors. Never mutate these; build a new *Error to add details.
var (
	ErrUnauthenticated = &Error{Kind: KindUnauthenticated, Code: "unauthenticated",
		Message: "Sign in to continue."}
	// ErrInvalidCredentials is deliberately the same for every reason a
	// sign-in fails: unknown practice, unknown email, wrong password, locked,
	// suspended. Distinguishing them lets anyone enumerate accounts.
	ErrInvalidCredentials = &Error{Kind: KindUnauthenticated, Code: "invalid_credentials",
		Message: "Sign-in failed. Check the practice code, email address and password."}
	ErrForbidden = &Error{Kind: KindForbidden, Code: "forbidden",
		Message: "Your role does not allow this."}
	ErrNotFound = &Error{Kind: KindNotFound, Code: "not_found",
		Message: "Not found."}

	ErrMFARequired = &Error{Kind: KindForbidden, Code: "mfa_required",
		Message: "Enter the code from your authenticator app to finish signing in."}
	ErrMFASetupRequired = &Error{Kind: KindForbidden, Code: "mfa_setup_required",
		Message: "Set up two-step verification before continuing."}
	ErrInvalidMFACode = &Error{Kind: KindUnauthenticated, Code: "invalid_code",
		Message: "That code is not correct. Codes change every 30 seconds and each one works once."}
	ErrMFAAlreadyEnabled = &Error{Kind: KindConflict, Code: "mfa_already_enabled",
		Message: "Two-step verification is already on. An administrator can reset it."}
	ErrMFANotEnabled = &Error{Kind: KindConflict, Code: "mfa_not_enabled",
		Message: "Two-step verification is not on for this account."}
	ErrMFAEnrolmentNotStarted = &Error{Kind: KindConflict, Code: "mfa_enrolment_not_started",
		Message: "Start setting up two-step verification first."}
	ErrNotPending = &Error{Kind: KindConflict, Code: "no_pending_sign_in",
		Message: "There is no sign-in waiting for a code."}

	ErrEmailTaken = &Error{Kind: KindConflict, Code: "email_taken",
		Message: "Another account at this practice already uses that email address."}
	ErrPatientHasLogin = &Error{Kind: KindConflict, Code: "patient_has_login",
		Message: "That patient already has a login."}
	ErrSelfAction = &Error{Kind: KindForbidden, Code: "self_action",
		Message: "Ask another administrator to do this for your own account."}
	ErrWrongPassword = &Error{Kind: KindInvalid, Code: "wrong_password",
		Message: "Your current password is not correct.",
		Fields:  []model.FieldError{{Field: "current_password", Message: "is not correct"}}}
	ErrNationalIDExists = &Error{Kind: KindConflict, Code: "national_id_exists",
		Message: "A patient with this ID number is already registered at this practice."}
	ErrSlugTaken = &Error{Kind: KindConflict, Code: "slug_taken",
		Message: "That practice code is already in use."}
)

func invalid(v model.ValidationErrors) error {
	return &Error{Kind: KindInvalid, Code: "validation_failed", Message: "Some fields need attention.", Fields: v}
}

func invalidField(field, message string) error {
	return invalid(model.ValidationErrors{{Field: field, Message: message}})
}

// fromRepo maps repository errors that callers can act on; anything else is
// returned unchanged and becomes a 500.
func fromRepo(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, repository.ErrNotFound), errors.Is(err, repository.ErrReference):
		// A foreign key rejection means the parent is not in this practice,
		// which from the caller's side is the same as not existing.
		return ErrNotFound
	}
	return err
}
