package plan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Reason is a stable, machine-readable failure cause. It is the only failure
// detail the Service keeps; the error text of the underlying transport is never
// surfaced.
type Reason string

// Known reasons.
const (
	ReasonNone             Reason = ""
	ReasonNetwork          Reason = "network"
	ReasonCanceled         Reason = "canceled"
	ReasonUnauthorized     Reason = "unauthorized"
	ReasonForbidden        Reason = "forbidden"
	ReasonRateLimited      Reason = "rate_limited"
	ReasonRedirectRefused  Reason = "redirect_refused"
	ReasonHTTPStatus       Reason = "http_status"
	ReasonMalformed        Reason = "malformed_response"
	ReasonNoCredentials    Reason = "no_credentials"
	ReasonNoWindows        Reason = "no_windows"
	ReasonTokenExpired     Reason = "token_expired"
	ReasonSourceMissing    Reason = "source_missing"
	ReasonSourceUnreadable Reason = "source_unreadable"
	ReasonUnsupported      Reason = "unsupported"
	ReasonNoData           Reason = "no_data"
)

var reasonText = map[Reason]string{
	ReasonNetwork:          "the quota request failed",
	ReasonCanceled:         "the quota request was canceled",
	ReasonUnauthorized:     "the provider rejected the stored credential",
	ReasonForbidden:        "the provider refused this account",
	ReasonRateLimited:      "the provider rate limited the quota request",
	ReasonRedirectRefused:  "the provider redirected the quota request; redirects are refused",
	ReasonHTTPStatus:       "the quota endpoint returned an unexpected status",
	ReasonMalformed:        "the quota response could not be decoded",
	ReasonNoCredentials:    "no stored credential was found",
	ReasonNoWindows:        "the response carried no quota window",
	ReasonTokenExpired:     "the stored credential is expired",
	ReasonSourceMissing:    "the credential source was not found",
	ReasonSourceUnreadable: "the credential source could not be read",
	ReasonUnsupported:      "this platform or installation is not supported",
	ReasonNoData:           "no quota data is available",
}

// Text returns the fixed, user-facing message for the reason.
func (r Reason) Text() string {
	if msg := reasonText[r]; msg != "" {
		return msg
	}
	if r == ReasonNone {
		return ""
	}
	return "quota unavailable"
}

// Error is a sanitized quota failure. Error() is a fixed message for its
// Reason; the unsanitized cause is reachable only through Unwrap, for tests and
// deliberate inspection.
type Error struct {
	Reason     Reason
	StatusCode int
	RetryAfter time.Duration

	cause error
}

// Error returns the fixed, sanitized message.
func (e *Error) Error() string {
	if e == nil {
		return "plan: <nil>"
	}
	msg := reasonText[e.Reason]
	if msg == "" {
		msg = "quota failed"
	}
	return "plan: " + string(e.Reason) + ": " + msg
}

// Unwrap exposes the unsanitized cause.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// QuotaReason implements Reasoner.
func (e *Error) QuotaReason() string { return string(e.Reason) }

// RetryDelay implements RetryDelayer.
func (e *Error) RetryDelay() time.Duration { return e.RetryAfter }

// NewError wraps a sanitized reason around a cause.
func NewError(reason Reason, cause error) *Error {
	return &Error{Reason: reason, cause: cause}
}

// IsReason reports whether err's chain carries a *Error with the given reason.
func IsReason(err error, reason Reason) bool {
	var e *Error
	return errors.As(err, &e) && e.Reason == reason
}

// Transport performs one HTTP request. It is injected so tests never use the
// network.
type Transport interface {
	Do(req *http.Request) (*http.Response, error)
}

// DefaultTimeout bounds one quota request.
const DefaultTimeout = 20 * time.Second

// DefaultMaxBytes bounds one quota response body.
const DefaultMaxBytes = 64 << 10

// DefaultTransport refuses every redirect, so a cross-origin redirect can
// never receive a credential, and bounds one request by DefaultTimeout.
func DefaultTransport() Transport {
	return &http.Client{
		Timeout: DefaultTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// DoJSON performs one bounded request and returns the response body. The target
// must be HTTPS unless it points at a loopback address, which is how tests use
// an httptest server. A response from a different URL than the one requested
// (a redirect a custom Transport followed) is refused.
func DoJSON(ctx context.Context, tp Transport, method, target string, header http.Header, body []byte, maxBytes int64) ([]byte, error) {
	if tp == nil {
		return nil, NewError(ReasonNetwork, nil)
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	u, err := url.Parse(target)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname()))) {
		return nil, NewError(ReasonUnsupported, err)
	}
	var rdr io.Reader
	if body != nil {
		rdr = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return nil, NewError(ReasonUnsupported, err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}

	resp, err := tp.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, NewError(ReasonCanceled, err)
		}
		return nil, NewError(ReasonNetwork, err)
	}
	defer resp.Body.Close()

	if resp.Request != nil && resp.Request.URL != nil && resp.Request.URL.String() != u.String() {
		return nil, &Error{Reason: ReasonRedirectRefused, StatusCode: resp.StatusCode}
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, &Error{Reason: ReasonUnauthorized, StatusCode: resp.StatusCode}
	case resp.StatusCode == http.StatusForbidden:
		return nil, &Error{Reason: ReasonForbidden, StatusCode: resp.StatusCode}
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &Error{Reason: ReasonRateLimited, StatusCode: resp.StatusCode, RetryAfter: retryAfter(resp)}
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, &Error{Reason: ReasonRedirectRefused, StatusCode: resp.StatusCode}
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, &Error{Reason: ReasonHTTPStatus, StatusCode: resp.StatusCode}
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, NewError(ReasonCanceled, err)
		}
		return nil, NewError(ReasonNetwork, err)
	}
	if int64(len(raw)) > maxBytes {
		return nil, NewError(ReasonMalformed, nil)
	}
	return raw, nil
}

func retryAfter(resp *http.Response) time.Duration {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// describe is used by provider tests to render a failure compactly.
func describe(err error) string {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return fmt.Sprintf("%s (status %d)", e.Reason, e.StatusCode)
	}
	return err.Error()
}
