package api

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/DDD0s/Klaud-Kode/Komputer/internal/backend"
	"github.com/DDD0s/Klaud-Kode/Komputer/internal/domain"
)

func httpErr(status int, message string) *domain.APIError {
	return &domain.APIError{Status: status, Message: message}
}

func typedErr(status int, message, typ string) *domain.APIError {
	return &domain.APIError{Status: status, Message: message, Type: typ}
}

var publicErrors = map[string]string{
	"upstream_auth_error":  "Authentication is unavailable. Ask the service operator to sign in again.",
	"rate_limit_error":     "Usage limit reached. Please retry later.",
	"overloaded_error":     "The service is busy. Please retry later.",
	"session_conflict":     "This conversation is busy. Please retry later.",
	"session_not_found":    "Conversation history could not be restored. Please retry with the conversation history.",
	"upstream_error":       "The request could not be completed. Please retry or contact the service operator.",
	"body_unavailable":     "The computer is unavailable. Please retry later.",
	"timeout_error":        "The request timed out. Please check its result before retrying.",
	"tool_result_timeout":  "Timed out waiting for tool results. Do not repeat completed operations automatically.",
	"tool_choice_error":    "The requested tool choice could not be satisfied.",
	"tool_output_limit":    "The tool cycle exceeded its output limit.",
	"builtin_tool_blocked": "The model used a tool on the service host instead of your computer, so the turn was stopped. Check what ran before retrying.",
	"not_implemented":      "This capability is not yet implemented in the Go rewrite.",
}

var errorStatus = map[string]int{
	"session_not_found":    502,
	"upstream_auth_error":  502,
	"rate_limit_error":     429,
	"overloaded_error":     503,
	"session_conflict":     409,
	"body_unavailable":     503,
	"upstream_error":       502,
	"builtin_tool_blocked": 502,
}

func publicMessage(err error) string {
	var apiErr *domain.APIError
	if errors.As(err, &apiErr) {
		if msg, ok := publicErrors[apiErr.Type]; ok {
			return msg
		}
		if apiErr.Status < 500 {
			return apiErr.Message
		}
		return "Internal service error. Please contact the service operator."
	}
	return "Internal service error. Please contact the service operator."
}

func errorType(err error) string {
	var apiErr *domain.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Type != "" {
			return apiErr.Type
		}
		switch {
		case apiErr.Status == 429:
			return "rate_limit_error"
		case apiErr.Status < 500:
			return "invalid_request_error"
		default:
			return "server_error"
		}
	}
	return "server_error"
}

func errorStatusOf(err error) int {
	var apiErr *domain.APIError
	if errors.As(err, &apiErr) && apiErr.Status != 0 {
		return apiErr.Status
	}
	return 500
}

var (
	authFailurePattern = regexp.MustCompile(`(?i)oauth|authenticat|not logged in|please run /login|invalid api key`)
	rateLimitPattern   = regexp.MustCompile(`(?i)usage limit|rate.?limit|limit reached|too many requests`)
	overloadedPattern  = regexp.MustCompile(`(?i)overloaded`)
	conflictPattern    = regexp.MustCompile(`(?i)already in use`)
)

// ClassifyCLIError maps a backend failure to a public API error.
func ClassifyCLIError(message string, b backend.Backend, explicitType string, activity bool) *domain.APIError {
	msg := domain.Redact(message)
	if len(msg) > 1200 {
		msg = msg[:1200]
	}
	classified := func(t string) *domain.APIError {
		status, ok := errorStatus[t]
		if !ok {
			status = 502
		}
		return &domain.APIError{
			Status: status, Type: t, Message: publicErrors[t], Detail: msg,
			MissingBeforeActivity: t == "session_not_found" && !activity,
		}
	}
	if explicitType != "" {
		return classified(explicitType)
	}
	if b != nil {
		if own := b.Classify(message); own != "" {
			return classified(own)
		}
	}
	switch {
	case authFailurePattern.MatchString(msg):
		return classified("upstream_auth_error")
	case rateLimitPattern.MatchString(msg):
		return classified("rate_limit_error")
	case overloadedPattern.MatchString(msg):
		return classified("overloaded_error")
	case conflictPattern.MatchString(msg):
		return classified("session_conflict")
	default:
		return classified("upstream_error")
	}
}

func privateDetail(err error) string {
	var apiErr *domain.APIError
	if errors.As(err, &apiErr) && apiErr.Detail != "" {
		return domain.Redact(apiErr.Detail)
	}
	return domain.Redact(err.Error())
}

func writeHTTPError(w http.ResponseWriter, path string, err error) {
	status := errorStatusOf(err)
	if status == 499 {
		return
	}
	headers := map[string]string{}
	var apiErr *domain.APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		headers["Retry-After"] = strconv.Itoa(apiErr.RetryAfter)
	} else if status == 429 {
		headers["Retry-After"] = "60"
	} else if status == 503 {
		headers["Retry-After"] = "10"
	}
	typ := errorType(err)
	msg := publicMessage(err)
	if isMessagesPath(path) {
		writeJSON(w, status, map[string]any{
			"type":  "error",
			"error": map[string]any{"type": typ, "message": msg},
		}, headers)
		return
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": typ},
	}, headers)
}

func isMessagesPath(path string) bool {
	return strings.Contains(path, "/messages")
}
