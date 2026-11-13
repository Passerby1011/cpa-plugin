package plugin

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"github.com/hex-ci/cpa-plugin/commandcode/internal/adapter/shared"
	"github.com/hex-ci/cpa-plugin/commandcode/internal/config"
	"github.com/hex-ci/cpa-plugin/commandcode/internal/errclass"
)

// zdrHeader is the header that asks for Zero Data Retention, exactly as the
// vendor CLI spells it (command-code 1.74.1 onward sends it on both of its
// surfaces, captured through a proxy).
const zdrHeader = "x-cmd-zdr"

// zdrValue is the only value the upstream reads as "yes": the CLI sends the
// literal "1", so anything else is not a request for ZDR at all.
const zdrValue = "1"

// zdrActive reports whether this request asks for Zero Data Retention.
//
// Two inputs, one answer, and it is deliberately re-read for EVERY request:
//
//   - config `zdr: true`, read from the snapshot the request already carries
//     (so flipping it takes effect when the operator saves, without a
//     restart); and
//   - env CMD_ZDR=1, read live at request time as the vendor CLI reads it.
//
// The env is re-read rather than captured at registration because it is the
// escape hatch an operator reaches for to turn ZDR on for a run WITHOUT
// writing config: capturing it at startup would make that knob silently
// ineffective until a restart, which is exactly the failure mode nobody debugs
// (they set the variable, saw no header, and concluded the plugin ignores
// CMD_ZDR). Only the exact string "1" counts, matching the CLI; "true" is not
// a request for ZDR upstream and must not be treated as one here either.
func zdrActive(cfg config.Config) bool {
	if cfg.ZDR {
		return true
	}
	return os.Getenv(config.EnvZDR) == "1"
}

// withZDR adds the ZDR request header to a header set when the switch is on.
// It takes the caller's header by value semantics (mutates and returns the
// same map) so call sites can stay one-liners around the builder they already
// use. A header is only ever ADDED: nothing here removes one, so a caller that
// set it for its own reasons keeps it.
func withZDR(h http.Header, cfg config.Config) http.Header {
	if !zdrActive(cfg) {
		return h
	}
	if h == nil {
		h = http.Header{}
	}
	h.Set(zdrHeader, zdrValue)
	return h
}

// zdrRefusedInError reports whether an upstream error body is the upstream's
// own "this model has no zero-data-retention upstream" refusal.
//
// The two surfaces report it differently and both must be recognised, because
// they carry the guarantee the client asked for:
//
//   - Provider API: HTTP 422 with {"error":{"code":"cmd_zdr_no_providers",...}}
//   - /alpha/generate: a 400 error line with {"error":{"type":"cmd_zdr_no_providers",
//     "code":"CMD_ZDR_NO_PROVIDERS",...}}
//
// Detection is deliberately by the documented code string, not by phrasing:
// the message text is prose the vendor can reword, while the code is the
// contract. The check is a plain case-insensitive substring on the bounded body
// so it works on both JSON shapes without a second decoder.
func zdrRefusedInError(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "cmd_zdr_no_providers")
}

// zdrRefusalError renders the upstream's ZDR refusal to the client as it came.
//
// WHY IT IS NOT A GENERIC 4xx: turning ZDR off and retrying would produce a
// cheerful answer to a request that explicitly asked not to be retained -
// silently defeating the only thing the client asked for. So the refusal is
// surfaced with the upstream's own status and message, and marked
// non-retryable so nothing upstream of this plugin "helpfully" re-runs it with
// different semantics.
//
// status is what the CLIENT should see (the Provider API's 422 stays 422; the
// CLI's 400 stays 400 after MapUpstreamStatus), and body is the upstream
// answer, redacted like every other upstream echo.
func zdrRefusalError(status int, body []byte) *errclass.Error {
	return &errclass.Error{
		Class:      errclass.ClassUnsupported,
		Message:    zdrRefusalMessage(body),
		StatusCode: status,
		Retryable:  false,
	}
}

// zdrRefusalMessage extracts the actionable sentence from a ZDR refusal.
//
// Both surfaces carry the same sentence under error.message; the prose is
// worth keeping because it tells the operator what to do ("Remove the
// x-cmd-zdr header or choose a different model" / "Disable CMD_ZDR..."). When
// no message is present the redacted body is the best remaining evidence -
// never an empty string, because an empty error message reads as a plugin
// fault rather than an upstream refusal.
func zdrRefusalMessage(body []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		for _, candidate := range []string{envelope.Error.Message, envelope.Message} {
			if msg := strings.TrimSpace(candidate); msg != "" {
				return errclass.Redact(msg)
			}
		}
	}
	return shared.RedactedSnippet(string(body))
}
