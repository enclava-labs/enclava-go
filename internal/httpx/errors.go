package httpx

import (
	"net/http"
	"strings"
)

type OpenAIErrorEnvelope struct {
	Error OpenAIError `json:"error"`
}

type OpenAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

func WriteOpenAIError(w http.ResponseWriter, status int, message, errType, code string) {
	if errType == "" {
		errType = "invalid_request_error"
	}
	WriteJSON(w, status, OpenAIErrorEnvelope{
		Error: OpenAIError{
			Message: message,
			Type:    errType,
			Code:    code,
		},
	})
}

// WritePriceMissing emits the 422 the missing-price sentinel maps to: the
// unit token stays in the message so the server and client error→action maps
// key on it; the internal "billing:" prefix never reaches the client.
func WritePriceMissing(w http.ResponseWriter, message string) {
	msg := strings.TrimSpace(strings.TrimPrefix(message, "billing: "))
	if msg == "" {
		msg = "required price missing"
	}
	WriteOpenAIError(w, http.StatusUnprocessableEntity, msg, "invalid_request_error", "price_missing")
}

func WriteMethodNotAllowed(w http.ResponseWriter) {
	WriteOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
}

func WriteUnauthorized(w http.ResponseWriter, message string) {
	WriteOpenAIError(w, http.StatusUnauthorized, message, "authentication_error", "invalid_api_key")
}

func WriteForbidden(w http.ResponseWriter, message string) {
	WriteOpenAIError(w, http.StatusForbidden, message, "permission_error", "forbidden")
}

func writeRateLimited(w http.ResponseWriter, dec RateLimitDecision) {
	if dec.Limit > 0 {
		w.Header().Set("X-RateLimit-Limit", itoa(dec.Limit))
		w.Header().Set("X-RateLimit-Remaining", itoa(dec.Remaining))
		w.Header().Set("X-RateLimit-Reset", itoa(dec.ResetEpoch))
	}
	if dec.RetryAfter > 0 {
		w.Header().Set("Retry-After", itoa(dec.RetryAfter))
	}
	WriteOpenAIError(w, http.StatusTooManyRequests, "rate limit exceeded", "rate_limit_error", "rate_limited")
}
