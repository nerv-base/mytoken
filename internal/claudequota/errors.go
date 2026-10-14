package claudequota

import (
	"errors"
	"fmt"
	"time"
)

// Reason is a stable, machine-readable cause. It is the only failure detail
// that reaches a caller, an error string or the GUI.
type Reason string

// Known reasons.
const (
	ReasonNone              Reason = ""
	ReasonNotEnabled        Reason = "not_enabled"
	ReasonConsentInvalid    Reason = "consent_invalid"
	ReasonOffline           Reason = "offline"
	ReasonDisabled          Reason = "disabled"
	ReasonThrottled         Reason = "throttled"
	ReasonNoSource          Reason = "no_source"
	ReasonStoreUnavailable  Reason = "store_unavailable"
	ReasonSourceMissing     Reason = "source_missing"
	ReasonSourceUnreadable  Reason = "source_unreadable"
	ReasonSourceUnsupported Reason = "source_unsupported"
	ReasonTokenMissing      Reason = "token_missing"
	ReasonTokenExpired      Reason = "token_expired"
	ReasonUnauthorized      Reason = "unauthorized"
	ReasonRateLimited       Reason = "rate_limited"
	ReasonRedirectRefused   Reason = "redirect_refused"
	ReasonHTTPStatus        Reason = "http_status"
	ReasonNetwork           Reason = "network"
	ReasonCanceled          Reason = "canceled"
	ReasonMalformed         Reason = "malformed_response"
	ReasonNoWindows         Reason = "no_windows"
)

// reasonText is the fixed, sanitized message for every reason. Nothing derived
// from a credential, a response body or a transport error is ever appended.
var reasonText = map[Reason]string{
	ReasonNotEnabled:        "Claude quota is not enabled for this instance; explicit consent is required",
	ReasonConsentInvalid:    "the stored Claude quota consent is unreadable; enable it again",
	ReasonOffline:           "Claude quota is unavailable in offline mode",
	ReasonDisabled:          "Claude quota was disabled while the request was in flight",
	ReasonThrottled:         "a Claude quota refresh is already in flight or too recent",
	ReasonNoSource:          "no Claude Code credential source is configured for this instance",
	ReasonStoreUnavailable:  "the settings store could not be read or written",
	ReasonSourceMissing:     "the Claude CLI credential source was not found",
	ReasonSourceUnreadable:  "the Claude CLI credential source could not be read",
	ReasonSourceUnsupported: "this credential source is not supported on this platform",
	ReasonTokenMissing:      "the credential source carries no OAuth access token",
	ReasonTokenExpired:      "the Claude CLI OAuth token is expired; renew it with the Claude CLI, then refresh",
	ReasonUnauthorized:      "the usage endpoint rejected the credential; renew it with the Claude CLI",
	ReasonRateLimited:       "the usage endpoint rate limited this request",
	ReasonRedirectRefused:   "the usage endpoint redirected the request; redirects are refused",
	ReasonHTTPStatus:        "the usage endpoint returned an unexpected status",
	ReasonNetwork:           "the usage request failed",
	ReasonCanceled:          "the usage request was canceled",
	ReasonMalformed:         "the usage response could not be decoded",
	ReasonNoWindows:         "the usage response carried no quota window",
}

// Error is a sanitized failure. Error() never includes a credential, a response
// body, a URL query or a transport message; the unsanitized cause is reachable
// only through Unwrap, and callers that surface it own that text.
type Error struct {
	// Reason is the stable cause.
	Reason Reason
	// Instance is the instance id the failure belongs to ("" when not
	// instance-specific).
	Instance string
	// Ref is the non-secret credential source reference (a path or a keychain
	// service), when the failure came from a source.
	Ref string
	// StatusCode is the HTTP status that caused the failure (0 when none). It
	// is a number, not response text.
	StatusCode int
	// RetryAfter is how long the caller should wait before the next attempt.
	RetryAfter time.Duration

	cause error
}

// Error returns the fixed, sanitized message for this reason.
func (e *Error) Error() string {
	if e == nil {
		return "claudequota: <nil>"
	}
	msg := reasonText[e.Reason]
	if msg == "" {
		msg = "Claude quota failed"
	}
	if e.Instance == "" {
		return "claudequota: " + string(e.Reason) + ": " + msg
	}
	return "claudequota: " + string(e.Reason) + " instance=" + e.Instance + ": " + msg
}

// Unwrap exposes the unsanitized cause for deliberate inspection only.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Is matches another *Error by reason, so errors.Is works against the package
// sentinels.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) || t == nil || e == nil {
		return false
	}
	return t.Reason == e.Reason
}

// Format makes %s, %v and %q render the sanitized message, never the cause.
func (e *Error) Format(f fmt.State, verb rune) {
	_, _ = f.Write([]byte(e.Error()))
}

// Package sentinels for the failures a caller branches on.
var (
	ErrNotEnabled   = &Error{Reason: ReasonNotEnabled}
	ErrOffline      = &Error{Reason: ReasonOffline}
	ErrDisabled     = &Error{Reason: ReasonDisabled}
	ErrThrottled    = &Error{Reason: ReasonThrottled}
	ErrNoSource     = &Error{Reason: ReasonNoSource}
	ErrUnauthorized = &Error{Reason: ReasonUnauthorized}
	ErrTokenExpired = &Error{Reason: ReasonTokenExpired}
	ErrRateLimited  = &Error{Reason: ReasonRateLimited}
)

// errReason builds a reason-only error.
func errReason(r Reason) *Error { return &Error{Reason: r} }

// asError reports whether err's chain carries an *Error, storing the innermost
// match in target.
func asError(err error, target **Error) bool {
	if err == nil {
		return false
	}
	return errors.As(err, target)
}

// withInstance copies a reason-only error with instance context attached.
func withInstance(err error, instance, ref string) error {
	var e *Error
	if !asError(err, &e) {
		return err
	}
	cp := *e
	cp.Instance = instance
	if ref != "" {
		cp.Ref = ref
	}
	return &cp
}
