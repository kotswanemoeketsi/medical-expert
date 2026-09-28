package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/service"
)

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyPrincipal
)

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(keyRequestID).(string)
	return id
}

// principalFrom returns the authenticated caller. Handlers behind
// requireAuth can rely on it being present.
func principalFrom(r *http.Request) *service.Principal {
	p, _ := r.Context().Value(keyPrincipal).(*service.Principal)
	return p
}

func newRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("t%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// withRequestID tags every request with an ID that appears in the response,
// in the logs and in the audit trail, so a report can be traced end to end.
func (a *API) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), keyRequestID, id)))
	})
}

// securityHeaders applies to every response, errors included. Every API
// response carries patient data or could, so nothing is cacheable.
func (a *API) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("Pragma", "no-cache")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if a.opt.Production {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// accessLog records method, path, status and duration. Never the query
// string, which can hold a patient's name, and never a body.
func (a *API) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		level := slogLevelFor(rec.status, r.URL.Path)
		a.log.Log(r.Context(), level, "http",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"ms", time.Since(start).Milliseconds(), "request_id", requestIDFrom(r.Context()),
			"ip", clientIP(r, a.opt.TrustProxyHeaders))
	})
}

// recoverer turns a panic into a 500 rather than a dropped connection, and
// logs the stack. A panic in one request must not take down a process that
// is serving clinicians.
func (a *API) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				a.log.ErrorContext(r.Context(), "panic", "request_id", requestIDFrom(r.Context()),
					"panic", fmt.Sprint(v), "stack", string(debug.Stack()))
				a.fail(w, r, fmt.Errorf("panic: %v", v))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (a *API) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, a.opt.MaxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP returns the caller's address. X-Forwarded-For is believed only
// when configured, and then only its right-most entry — the one our own
// proxy appended. Entries to its left came from the client and can be
// anything.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (a *API) rateLimit(l *RateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, wait := l.Allow(clientIP(r, a.opt.TrustProxyHeaders))
		if !ok {
			secs := int(math.Ceil(wait.Seconds()))
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			writeJSON(w, http.StatusTooManyRequests, errorEnvelope{Error: map[string]any{
				"code": "rate_limited", "message": "Too many attempts. Wait a minute and try again.",
				"request_id": requestIDFrom(r.Context()),
			}})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireAuth resolves the bearer token and then applies the MFA gate. Every
// authenticated route passes through here, so the gate cannot be forgotten
// on a new route.
func (a *API) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		p, err := a.reg.Authenticate(r.Context(), token, service.RequestMeta{
			RequestID: requestIDFrom(r.Context()),
			IP:        clientIP(r, a.opt.TrustProxyHeaders),
			UserAgent: r.UserAgent(),
		})
		if err != nil {
			a.fail(w, r, err)
			return
		}
		if err := mfaGate(p.State, r.Method, r.URL.Path); err != nil {
			a.fail(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), keyPrincipal, p)))
	})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// The only routes a half-signed-in session may reach. Everything else — read
// or write — is refused, so a password alone never reaches patient data.
var (
	pendingAllowed = map[string]bool{
		"POST /api/v1/auth/mfa/verify": true,
		"POST /api/v1/auth/logout":     true,
	}
	setupAllowed = map[string]bool{
		"GET /api/v1/me":                      true,
		"GET /api/v1/auth/mfa/status":         true,
		"POST /api/v1/auth/mfa/enrol":         true,
		"POST /api/v1/auth/mfa/enrol/confirm": true,
		"POST /api/v1/auth/password":          true,
		"POST /api/v1/auth/logout":            true,
	}
)

func mfaGate(state service.SessionState, method, path string) error {
	key := method + " " + path
	switch state {
	case service.StateActive:
		if key == "POST /api/v1/auth/mfa/verify" {
			return service.ErrNotPending
		}
		return nil
	case service.StatePending:
		if pendingAllowed[key] {
			return nil
		}
		return service.ErrMFARequired
	case service.StateSetup:
		if setupAllowed[key] {
			return nil
		}
		return service.ErrMFASetupRequired
	}
	return service.ErrUnauthenticated
}

// requireRole is the coarse check. Finer rules — a patient reading only
// their own record — live in the services, because they need data to decide.
func (a *API) requireRole(roles []model.Role, next http.Handler) http.Handler {
	allowed := map[model.Role]bool{}
	for _, r := range roles {
		allowed[r] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r)
		if p == nil || !allowed[p.Actor.Role] {
			a.fail(w, r, service.ErrForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
