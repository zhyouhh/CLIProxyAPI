package management

import (
	"encoding/json"
	"math"
	"strings"
)

type codexRecoveryWindow struct {
	Used    *float64 `json:"used_percent"`
	Seconds *int64   `json:"limit_window_seconds"`
}
type codexRecoveryLimit struct {
	Allowed   *bool                `json:"allowed"`
	Reached   *bool                `json:"limit_reached"`
	Primary   *codexRecoveryWindow `json:"primary_window"`
	Secondary *codexRecoveryWindow `json:"secondary_window"`
}

func (l *codexRecoveryLimit) available() bool {
	if l == nil || l.Allowed == nil || !*l.Allowed || l.Reached == nil || *l.Reached {
		return false
	}
	active := false
	for _, w := range []*codexRecoveryWindow{l.Primary, l.Secondary} {
		if w == nil {
			continue
		}
		if w.Seconds == nil || *w.Seconds < 0 {
			return false
		}
		if *w.Seconds == 0 {
			continue
		}
		active = true
		if w.Used == nil || math.IsNaN(*w.Used) || *w.Used < 0 || *w.Used >= 100 {
			return false
		}
	}
	return active
}

// Recovery requires explicit headroom in every advertised quota, including
// model-specific pools. Missing/malformed data never grants availability.
func codexUsageAvailable(body []byte) bool {
	var p struct {
		Limit      *codexRecoveryLimit `json:"rate_limit"`
		Additional []struct {
			Limit *codexRecoveryLimit `json:"rate_limit"`
		} `json:"additional_rate_limits"`
	}
	if json.Unmarshal(body, &p) != nil || !p.Limit.available() {
		return false
	}
	for _, l := range p.Additional {
		if !l.Limit.available() {
			return false
		}
	}
	return true
}

// Only the selected auth token may determine the usage account. In particular,
// reject differently cased duplicates, cookies and caller-selected account IDs.
func codexRecoveryHeaders(headers map[string]string) bool {
	seen := map[string]bool{}
	for key, value := range headers {
		name := strings.ToLower(key)
		if seen[name] {
			return false
		}
		seen[name] = true
		switch name {
		case "authorization":
			if value != "Bearer $TOKEN$" {
				return false
			}
		case "content-type", "user-agent":
		default:
			return false
		}
	}
	return seen["authorization"]
}
