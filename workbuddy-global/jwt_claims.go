// jwt_claims.go holds the minimal, unverified JWT payload reader used for
// routing decisions.
//
// SECURITY: nothing here validates a signature, an expiry, or an audience. It
// exists only to read the `iss` claim so a credential is sent to the gateway
// that minted it. Authorization is always performed by the upstream, which
// verifies the token it receives. Never use these helpers to gate access.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// jwtRoutingClaims is the subset of claims used for routing.
type jwtRoutingClaims struct {
	Issuer string `json:"iss"`
	// GivenName is carried for display purposes (enterprise accounts) and is
	// never used for routing.
	GivenName string `json:"given_name"`
}

// decodeUnverifiedJWTClaims parses the payload segment of a JWT without
// verifying it.
//
// Both base64url encodings are accepted: the JWT spec forbids padding but
// implementations vary, and some upstreams emit either form. The payload is
// re-padded before decoding to avoid a spurious failure on a legitimate token.
func decodeUnverifiedJWTClaims(accessToken string) (jwtRoutingClaims, error) {
	var claims jwtRoutingClaims
	token := strings.TrimSpace(accessToken)
	if token == "" {
		return claims, fmt.Errorf("access token is empty")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims, fmt.Errorf("access token is not a JWT")
	}
	raw, err := decodeJWTSegment(parts[1])
	if err != nil {
		return claims, fmt.Errorf("decode JWT payload: %w", err)
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return claims, fmt.Errorf("decode JWT claims: %w", err)
	}
	return claims, nil
}

// decodeJWTSegment decodes one base64url segment, tolerating either padding
// convention.
func decodeJWTSegment(segment string) ([]byte, error) {
	if segment == "" {
		return nil, fmt.Errorf("empty JWT segment")
	}
	if raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(segment, "=")); err == nil {
		return raw, nil
	}
	// Fall back to the padded decoder for segments emitted with '=' suffixes or
	// with the standard (+/) alphabet.
	if pad := len(segment) % 4; pad != 0 {
		segment += strings.Repeat("=", 4-pad)
	}
	return base64.URLEncoding.DecodeString(segment)
}

// parseIssuerHost extracts the lowercase hostname from an `iss` claim.
//
// The issuer is expected to be an absolute URL. A bare hostname is tolerated
// because some tokens carry one, and rejecting it would misroute a valid
// credential over a formatting detail that does not affect trust.
func parseIssuerHost(issuer string) (string, error) {
	trimmed := strings.TrimSpace(issuer)
	if trimmed == "" {
		return "", fmt.Errorf("JWT issuer is empty")
	}
	if !strings.Contains(trimmed, "://") {
		// Treat as a bare host.
		host := strings.ToLower(strings.TrimSuffix(trimmed, "/"))
		if host == "" {
			return "", fmt.Errorf("JWT issuer host is empty")
		}
		return host, nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("JWT issuer is not a valid URL: %w", err)
	}
	if !parsed.IsAbs() || parsed.Hostname() == "" {
		return "", fmt.Errorf("JWT issuer is not an absolute URL")
	}
	return strings.ToLower(parsed.Hostname()), nil
}

// jwtGivenNameValue reads the given_name claim, used for enterprise display
// names. Returns "" when the token is absent or the claim is unset.
func jwtGivenNameValue(accessToken string) string {
	claims, err := decodeUnverifiedJWTClaims(accessToken)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(claims.GivenName)
}
