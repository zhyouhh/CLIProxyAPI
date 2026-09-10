package auth

import (
	"context"
	"encoding/json"
	"strings"
)

// RecoverCodexQuota applies an authenticated usage observation only if no request
// result or credential replacement has changed this auth while the probe ran.
func (m *Manager) RecoverCodexQuota(ctx context.Context, id string, epoch, generation uint64) (*Auth, []string, error) {
	return m.resetQuota(ctx, id, &Auth{RegistrationEpoch: epoch, Generation: generation})
}

func recoverableCodexQuota(q QuotaState, last *Error) bool {
	if !q.Exceeded || (q.Reason != "quota" && q.Reason != "credential_quota") || last == nil || last.HTTPStatus != 429 {
		return false
	}
	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	return json.Unmarshal([]byte(last.Message), &payload) == nil && (strings.EqualFold(strings.TrimSpace(payload.Error.Type), "usage_limit_reached") || strings.EqualFold(strings.TrimSpace(payload.Type), "usage_limit_reached"))
}
