package management

import (
	"strings"
	"testing"
)

func TestCodexUsageRecoveryRequiresCompleteAvailableLimits(t *testing.T) {
	good := `{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":0,"limit_window_seconds":604800}},"additional_rate_limits":[]}`
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{good, true}, {`{}`, false}, {strings.Replace(good, `"allowed":true`, `"allowed":false`, 1), false},
		{strings.Replace(good, `"used_percent":0`, `"used_percent":100`, 1), false},
		{strings.Replace(good, `"used_percent":0`, `"used_percent":null`, 1), false},
		{strings.Replace(good, `"limit_reached":false`, `"limit_reached":true`, 1), false},
		{strings.Replace(good, `"additional_rate_limits":[]`, `"additional_rate_limits":[{"rate_limit":{"allowed":false,"limit_reached":true}}]`, 1), false},
	} {
		if got := codexUsageAvailable([]byte(tc.body)); got != tc.ok {
			t.Errorf("available=%v want %v body=%s", got, tc.ok, tc.body)
		}
	}
}

func TestCodexRecoveryRejectsCredentialOverrideHeaders(t *testing.T) {
	for _, headers := range []map[string]string{
		{"Authorization": "Bearer $TOKEN$", "authorization": "Bearer another-account"},
		{"Authorization": "Bearer $TOKEN$", "Chatgpt-Account-Id": "another-account"},
		{"Authorization": "Bearer $TOKEN$", "Cookie": "another-session"},
		{"Authorization": "Bearer literal-token"},
	} {
		if codexRecoveryHeaders(headers) {
			t.Fatal("accepted credential override")
		}
	}
	if !codexRecoveryHeaders(map[string]string{"authorization": "Bearer $TOKEN$", "User-Agent": "test"}) {
		t.Fatal("rejected canonical probe")
	}
}
