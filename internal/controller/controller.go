// Package controller is the HTTP layer. Handlers do three things: read the
// request, call a service, render the result. Business rules live in the
// service package; service errors become status codes in exactly one place,
// fail() below.
package controller

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/service"
)

// Options configure the HTTP layer.
type Options struct {
	Production bool
	// TrustProxyHeaders believes X-Forwarded-For. Only set it behind a
	// reverse proxy that overwrites the header; otherwise any client can
	// forge its address and walk past the rate limiter.
	TrustProxyHeaders bool
	LoginPerMinute    int // per source address
	MaxBodyBytes      int64
	// WhatsAppAppSecret checks webhook signatures. Empty refuses every
	// webhook rather than trusting unsigned ones.
	WhatsAppAppSecret string
	// WhatsAppVerifyToken answers Meta's subscription check.
	WhatsAppVerifyToken string
}

// API holds what handlers need.
type API struct {
	reg          *service.Registry
	log          *slog.Logger
	opt          Options
	loginLimiter *RateLimiter
	mfaLimiter   *RateLimiter
}

func New(reg *service.Registry, log *slog.Logger, opt Options) *API {
	if log == nil {
		log = slog.Default()
	}
	if opt.LoginPerMinute <= 0 {
		opt.LoginPerMinute = 10
	}
	if opt.MaxBodyBytes <= 0 {
		opt.MaxBodyBytes = 1 << 20
	}
	return &API{
		reg: reg, log: log, opt: opt,
		loginLimiter: NewRateLimiter(opt.LoginPerMinute, opt.LoginPerMinute),
		mfaLimiter:   NewRateLimiter(opt.LoginPerMinute, opt.LoginPerMinute),
	}
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}

// errorEnvelope is the one error shape every endpoint returns.
type errorEnvelope struct {
	Error map[string]any `json:"error"`
}

// badRequest is an HTTP-layer problem: the body could not be read at all.
type badRequest struct{ msg string }

func (e *badRequest) Error() string { return e.msg }

// fail renders an error. It is the only place a service error becomes a
// status code, so a new business error needs no handler changes.
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	reqID := requestIDFrom(r.Context())
	body := map[string]any{"request_id": reqID}

	var se *service.Error
	var br *badRequest
	var status int
	switch {
	case errors.As(err, &se):
		status = statusFor(se.Kind)
		body["code"], body["message"] = se.Code, se.Message
		if len(se.Fields) > 0 {
			body["fields"] = se.Fields
		}
		for k, v := range se.Details {
			if _, taken := body[k]; !taken {
				body[k] = v
			}
		}
		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mes"`)
		}
	case errors.As(err, &br):
		status = http.StatusBadRequest
		body["code"], body["message"] = "bad_request", br.msg
	default:
		status = http.StatusInternalServerError
		body["code"] = "internal_error"
		body["message"] = "Something went wrong on our side. It has been logged; quote reference " + reqID + " if you report it."
		a.log.ErrorContext(r.Context(), "request failed", "request_id", reqID, "method", r.Method,
			"path", r.URL.Path, "err", err)
	}
	writeJSON(w, status, errorEnvelope{Error: body})
}

func statusFor(k service.Kind) int {
	switch k {
	case service.KindInvalid:
		return http.StatusUnprocessableEntity
	case service.KindUnauthenticated:
		return http.StatusUnauthorized
	case service.KindForbidden:
		return http.StatusForbidden
	case service.KindNotFound:
		return http.StatusNotFound
	case service.KindConflict:
		return http.StatusConflict
	case service.KindUnavailable:
		return http.StatusServiceUnavailable
	case service.KindLimited:
		return http.StatusTooManyRequests
	}
	return http.StatusInternalServerError
}

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

// decode reads a JSON body strictly: unknown fields are an error, because in
// a clinical record a misspelt field name silently ignored ("alergies") is
// a worse outcome than a rejected request.
func decode(r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return &badRequest{"Send the body as application/json."}
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		switch {
		case errors.As(err, &maxErr):
			return &badRequest{"The request body is too large."}
		case errors.Is(err, io.EOF):
			return &badRequest{"The request body is empty."}
		case errors.As(err, &syntaxErr):
			return &badRequest{fmt.Sprintf("The request body is not valid JSON (at byte %d).", syntaxErr.Offset)}
		case errors.Is(err, io.ErrUnexpectedEOF):
			return &badRequest{"The request body is not valid JSON (it ends too early)."}
		case errors.As(err, &typeErr):
			return &badRequest{fmt.Sprintf("Field %q has the wrong type.", typeErr.Field)}
		case strings.HasPrefix(err.Error(), "json: unknown field"):
			return &badRequest{"Unknown field " + strings.TrimPrefix(err.Error(), "json: unknown field ") + "."}
		default:
			return &badRequest{"The request body could not be read: " + err.Error()}
		}
	}
	if dec.More() {
		return &badRequest{"The request body must contain a single JSON object."}
	}
	return nil
}

// decodeOptional is decode for endpoints whose body may be left out
// entirely, such as a cancellation with no reason. A request with no body at
// all is accepted whatever its Content-Type says: PowerShell, for one, labels
// a POST without a body as a form.
func decodeOptional(r *http.Request, dst any) error {
	body := bufio.NewReader(r.Body)
	if _, err := body.Peek(1); errors.Is(err, io.EOF) {
		return nil
	}
	r.Body = readCloser{body, r.Body}
	err := decode(r, dst)
	var br *badRequest
	if errors.As(err, &br) && br.msg == "The request body is empty." {
		return nil
	}
	return err
}

type readCloser struct {
	io.Reader
	io.Closer
}

// queryTime reads an RFC 3339 timestamp from the query string.
func queryTime(r *http.Request, name string) (time.Time, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, &service.Error{Kind: service.KindInvalid, Code: "validation_failed", Message: "Some fields need attention.",
			Fields: []model.FieldError{{Field: name, Message: "must be an RFC 3339 timestamp, e.g. 2026-09-28T08:00:00+02:00"}}}
	}
	return t, nil
}

// pathID reads an identifier from the path. Something that is not an ID
// cannot name a record, so it is simply not found.
func pathID(r *http.Request, name string) (model.ID, error) {
	id, err := model.ParseID(r.PathValue(name))
	if err != nil {
		return model.ID{}, service.ErrNotFound
	}
	return id, nil
}

func queryInt(r *http.Request, name string, def int) int {
	s := r.URL.Query().Get(name)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func queryID(r *http.Request, name string) (*model.ID, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return nil, nil
	}
	id, err := model.ParseID(s)
	if err != nil {
		return nil, &service.Error{Kind: service.KindInvalid, Code: "validation_failed", Message: "Some fields need attention.",
			Fields: []model.FieldError{{Field: name, Message: "is not a valid identifier"}}}
	}
	return &id, nil
}
