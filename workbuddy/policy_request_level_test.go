// policy_request_level_test.go covers the two request-level classes: 11102
// (this backend does not serve that model) and 11115 (prompt is too long).
//
// Both are verdicts about the request, not the credential. Rotating accounts or
// disabling one would discard a healthy credential for a condition that
// reproduces on every account.
package main

import "testing"

func TestClassifyModelBlocked(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   upstreamErrKind
	}{
		{
			name:   "11102 by code",
			status: 400,
			body:   `{"code":11102,"msg":"service info not found","requestId":"abc"}`,
			want:   upstreamErrModelBlocked,
		},
		{
			name:   "11102 by message on 404",
			status: 404,
			body:   `{"msg":"service info not found"}`,
			want:   upstreamErrModelBlocked,
		},
		{
			// A requestId that happens to contain the digits must NOT trigger the
			// class: matching the whole body would blacklist a usable model.
			name:   "11102 digits inside requestId do not match",
			status: 400,
			body:   `{"code":0,"msg":"ok","requestId":"11102abcdef"}`,
			want:   upstreamErrOther,
		},
		{
			// Only request-level statuses qualify.
			name:   "11102 code on a 500 is not model_blocked",
			status: 500,
			body:   `{"code":11102,"msg":"service info not found"}`,
			want:   upstreamErrServer,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyUpstreamError(tc.status, tc.body); got != tc.want {
				t.Fatalf("classify(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

func TestClassifyPromptTooLong(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   upstreamErrKind
	}{
		{
			name:   "11115 by code",
			status: 400,
			body:   `{"code":11115,"msg":"prompt is too long: 210000 tokens > 200000 maximum"}`,
			want:   upstreamErrPromptTooLong,
		},
		{
			name:   "11115 by wording on 413",
			status: 413,
			body:   `{"msg":"Prompt is too long"}`,
			want:   upstreamErrPromptTooLong,
		},
		{
			// A 429 carrying the same text is still a rate limit.
			name:   "prompt wording on 429 stays soft rate",
			status: 429,
			body:   `{"msg":"prompt is too long"}`,
			want:   upstreamErrSoftRate,
		},
		{
			// 5xx is an upstream fault, not a request verdict.
			name:   "prompt wording on 500 stays server",
			status: 500,
			body:   `{"msg":"prompt is too long"}`,
			want:   upstreamErrServer,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyUpstreamError(tc.status, tc.body); got != tc.want {
				t.Fatalf("classify(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// Both new classes must map to the non-punishing effect: the credential is
// healthy and must not be touched.
func TestRequestLevelClassesDoNotPunishCredential(t *testing.T) {
	for _, kind := range []upstreamErrKind{upstreamErrModelBlocked, upstreamErrPromptTooLong} {
		if got := executorErrorEffectFor(kind); got != executorEffectIgnore {
			t.Errorf("executorErrorEffectFor(%v) = %v, want ignore", kind, got)
		}
	}
}

func TestNewClassNames(t *testing.T) {
	if got := upstreamErrModelBlocked.String(); got != "model_blocked" {
		t.Errorf("model_blocked name = %q", got)
	}
	if got := upstreamErrPromptTooLong.String(); got != "prompt_too_long" {
		t.Errorf("prompt_too_long name = %q", got)
	}
}
