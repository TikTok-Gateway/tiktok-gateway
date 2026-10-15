package gateway

import (
	"errors"
	"regexp"
	"strings"

	g "tiktok-chat-bot/internal/greeter"
)

var (
	diagnosticURL         = regexp.MustCompile(`(?i)https?://[^\s"<>]+`)
	diagnosticBody        = regexp.MustCompile(`(?s)(API returned [0-9]{3}):.*`)
	diagnosticCredentials = regexp.MustCompile(`(?is)\b(?:authorization|proxy-authorization|cookie|set-cookie|sessionid|sessionid_ss|mstoken|access_token|refresh_token|tt-ticket-guard[^\s:=]*)\s*[:=].*`)
)

// Preserve the send failure context, but never persist HTTP response bodies,
// signed URLs, or credential headers. Bound the stored diagnostic size.
func sendErrorDiagnostic(err error) string {
	if err == nil {
		return "missing server message ID in send response"
	}
	var rejection *g.APIRejection
	if errors.As(err, &rejection) {
		return rejection.Error()
	}
	message := diagnosticBody.ReplaceAllString(err.Error(), "$1: [response body omitted]")
	message = diagnosticURL.ReplaceAllString(message, "[URL omitted]")
	message = diagnosticCredentials.ReplaceAllString(message, "[credentials omitted]")
	message = strings.Join(strings.Fields(message), " ")
	chars := []rune(message)
	if len(chars) > 2048 {
		message = string(chars[:2048]) + "…"
	}
	return message
}
