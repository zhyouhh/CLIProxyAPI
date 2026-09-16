package executor

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexEnvironmentContextOpen  = "<environment_context>"
	codexEnvironmentContextClose = "</environment_context>"
	// codexEnvironmentDateLayout matches the <current_date> format Codex clients emit.
	codexEnvironmentDateLayout = "2006-01-02"
	// codexEnvironmentMissLogInterval throttles the "configured but nothing matched" warning.
	codexEnvironmentMissLogInterval = 5 * time.Minute
)

// codexEnvironmentTimezoneKeys lists the credential field spellings that carry a
// per-credential IANA timezone, mirroring how credential groups accept aliases.
var codexEnvironmentTimezoneKeys = []string{"timezone", "time-zone", "time_zone"}

// codexEnvironmentMissLoggedAt holds the Unix nanosecond stamp of the last miss warning.
var codexEnvironmentMissLoggedAt atomic.Int64

// applyCodexEnvironmentContext rewrites the <timezone> and <current_date> values that Codex
// clients embed in their <environment_context> input item so they match the region the
// selected credential egresses from. Codex CLI and Codex Desktop both report the operating
// system timezone, which disagrees with a credential routed through a proxy_url elsewhere.
//
// The rewrite is opt-in and conservative: it is a no-op unless a timezone is configured, it
// only touches input items whose entire text is one environment context block, it only
// replaces tags that already carry a value, and an unparsable timezone disables it rather
// than guessing.
//
// <timezone> is rewritten in every block. It describes the machine, not the turn, so a single
// constant is both cache-stable across turns and internally consistent: rewriting only the
// live block would put two different timezones in one request, which reads as "this account
// moved" and is a worse signal than not rewriting at all.
//
// <current_date> describes when a turn happened, so it is shifted rather than replaced — see
// shiftCodexEnvironmentDate.
func applyCodexEnvironmentContext(cfg *config.Config, auth *cliproxyauth.Auth, rawJSON []byte) []byte {
	timezone := codexEnvironmentTimezone(cfg, auth)
	if timezone == "" || len(rawJSON) == 0 {
		return rawJSON
	}
	location, errLocation := time.LoadLocation(timezone)
	if errLocation != nil {
		log.Warnf("codex environment context: unusable timezone %q, leaving the payload untouched: %v", timezone, errLocation)
		return rawJSON
	}
	rewritten, matched := rewriteCodexEnvironmentContext(rawJSON, timezone, location)
	if !matched {
		logCodexEnvironmentMiss(timezone)
	}
	return rewritten
}

// logCodexEnvironmentMiss reports that a timezone is configured but no environment context
// block was found. Silence here would mean a client format change quietly reverts every
// request to leaking the operator's own timezone, so the miss has to be visible; the warning
// is throttled because it would otherwise fire once per request.
func logCodexEnvironmentMiss(timezone string) {
	now := time.Now().UnixNano()
	last := codexEnvironmentMissLoggedAt.Load()
	// now < last means the wall clock moved backwards, typically an NTP correction. Treat that
	// as "interval elapsed" rather than letting a stamp from the future mute the tripwire until
	// real time catches up.
	if last != 0 && now >= last && now-last < int64(codexEnvironmentMissLogInterval) {
		return
	}
	if !codexEnvironmentMissLoggedAt.CompareAndSwap(last, now) {
		return
	}
	log.Warnf("codex environment context: timezone %q is configured but no <environment_context> input item matched; the client format may have changed and requests still report their own timezone", timezone)
}

// codexEnvironmentTimezone resolves the timezone for the selected credential.
// A per-credential value wins over the global fallback; an empty result disables the rewrite.
func codexEnvironmentTimezone(cfg *config.Config, auth *cliproxyauth.Auth) string {
	if timezone := codexCredentialTimezone(auth); timezone != "" {
		return timezone
	}
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.CodexHeaderDefaults.Timezone)
}

// codexCredentialTimezone reads the timezone declared on the credential itself,
// either as a synthesized attribute or as a top-level auth file field.
func codexCredentialTimezone(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	for _, key := range codexEnvironmentTimezoneKeys {
		if auth.Attributes != nil {
			if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
				return value
			}
		}
		if auth.Metadata != nil {
			if value, ok := auth.Metadata[key].(string); ok {
				if trimmed := strings.TrimSpace(value); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	return ""
}

// codexEnvironmentEdit records one pending rewrite so the payload is rebuilt in a single pass.
type codexEnvironmentEdit struct {
	path string
	text string
}

// rewriteCodexEnvironmentContext rewrites every input item that is an environment context
// block. The second return value reports whether any such item was found at all, which is
// what distinguishes "nothing to do" from "the format moved out from under us".
func rewriteCodexEnvironmentContext(rawJSON []byte, timezone string, location *time.Location) ([]byte, bool) {
	input := gjson.GetBytes(rawJSON, "input")
	if !input.IsArray() {
		return rawJSON, false
	}
	matched := false
	edits := make([]codexEnvironmentEdit, 0, 4)
	for i, item := range input.Array() {
		content := item.Get("content")
		if content.Type == gjson.String {
			if !isCodexEnvironmentBlock(content.String()) {
				continue
			}
			matched = true
			if rewritten, changed := rewriteCodexEnvironmentBlock(content.String(), timezone, location); changed {
				edits = append(edits, codexEnvironmentEdit{path: fmt.Sprintf("input.%d.content", i), text: rewritten})
			}
			continue
		}
		if !content.IsArray() {
			continue
		}
		for j, part := range content.Array() {
			text := part.Get("text")
			if !text.Exists() || text.Type != gjson.String || !isCodexEnvironmentBlock(text.String()) {
				continue
			}
			matched = true
			if rewritten, changed := rewriteCodexEnvironmentBlock(text.String(), timezone, location); changed {
				edits = append(edits, codexEnvironmentEdit{path: fmt.Sprintf("input.%d.content.%d.text", i, j), text: rewritten})
			}
		}
	}
	if len(edits) == 0 {
		return rawJSON, matched
	}
	// Each edit copies the payload, so this is O(blocks x payload); real sessions carry at
	// most a handful of blocks. Keep the default allocating sjson call. Its in-place fast
	// path is banned repo-wide by TestNoInPlaceSJSONWrites because it corrupts no-copy gjson
	// results that alias the same buffer, and it is also wrong here for a second reason:
	// measured against this payload it returns the input unchanged, with a nil error, once
	// the replacement value needs JSON escaping — and an environment block always has
	// newlines, so the whole rewrite would silently become a no-op.
	updated := rawJSON
	for _, edit := range edits {
		next, errSet := sjson.SetBytes(updated, edit.path, edit.text)
		if errSet != nil {
			log.Warnf("codex environment context: could not rewrite %s: %v", edit.path, errSet)
			continue
		}
		updated = next
	}
	return updated, matched
}

// isCodexEnvironmentBlock reports whether a text value is one environment context block and
// nothing else. Requiring the whole value to be the block keeps the rewrite off conversation
// content that merely quotes one, such as a pasted session log.
func isCodexEnvironmentBlock(text string) bool {
	trimmed := strings.TrimSpace(text)
	return strings.HasPrefix(trimmed, codexEnvironmentContextOpen) && strings.HasSuffix(trimmed, codexEnvironmentContextClose)
}

// rewriteCodexEnvironmentBlock rewrites every environment context block inside one admitted
// text value. Admission only proves the value starts and ends with the delimiters, so it can
// still hold several concatenated blocks; rewriting just the first would leave the client's
// real timezone in the later ones and send both upstream in the same request.
func rewriteCodexEnvironmentBlock(text string, timezone string, location *time.Location) (string, bool) {
	var builder strings.Builder
	rest := text
	for {
		start := strings.Index(rest, codexEnvironmentContextOpen)
		if start < 0 {
			break
		}
		relativeEnd := strings.Index(rest[start:], codexEnvironmentContextClose)
		if relativeEnd < 0 {
			break
		}
		end := start + relativeEnd + len(codexEnvironmentContextClose)
		builder.WriteString(rest[:start])
		builder.WriteString(rewriteOneCodexEnvironmentBlock(rest[start:end], timezone, location))
		rest = rest[end:]
	}
	builder.WriteString(rest)
	rewritten := builder.String()
	return rewritten, rewritten != text
}

// rewriteOneCodexEnvironmentBlock shifts one block's date into the target timezone and then
// overwrites the reported timezone. The date is read before the timezone is replaced because
// it needs the block's original timezone to convert from.
func rewriteOneCodexEnvironmentBlock(block string, timezone string, location *time.Location) string {
	sourceTimezone := readCodexEnvironmentTag(block, "timezone")
	sourceDate := readCodexEnvironmentTag(block, "current_date")

	rewritten := block
	if shifted, ok := shiftCodexEnvironmentDate(sourceDate, sourceTimezone, location); ok {
		rewritten = replaceCodexEnvironmentTag(rewritten, "current_date", shifted)
	}
	return replaceCodexEnvironmentTag(rewritten, "timezone", timezone)
}

// shiftCodexEnvironmentDate converts a block's own date into the target timezone by anchoring
// it at noon of that day in the timezone the block reported.
//
// Anchoring at noon rather than using the real current time is deliberate. The result is a
// pure function of (date, source timezone, target timezone), so it stays byte-identical for
// as long as the client keeps sending the same date, and the rewritten prefix therefore flips
// exactly when the client's own value flips and never on its own schedule. Deriving the live
// block from time.Now() would instead move the flip to midnight in the target timezone —
// mid-afternoon for the operator — and invalidate the upstream prompt cache in the middle of
// a working session. The cost is that the reported date can be one day ahead of the target
// timezone's real date for part of the day; the whole conversation stays self-consistent.
//
// Historical blocks keep their own shifted date instead of collapsing onto today.
//
// One property is deployment-specific and must be re-checked before pointing a credential at
// a timezone east of the clients using it. Going west — the Asia/Shanghai to America/Los_Angeles
// case this was built for — the anchor always lands one day behind, and the true target date is
// either that same day or one later, so the reported date is never ahead of the target
// timezone's real date. A date in the past is indistinguishable from a session that started
// before midnight and is still running, which is Codex's normal shape; a date in the future has
// no such explanation. Going east the anchor can report a day ahead for part of the client's
// day, and that trade-off no longer holds.
func shiftCodexEnvironmentDate(date string, sourceTimezone string, target *time.Location) (string, bool) {
	if date == "" || sourceTimezone == "" || target == nil {
		return "", false
	}
	source, errLocation := time.LoadLocation(sourceTimezone)
	if errLocation != nil {
		return "", false
	}
	// Read the calendar fields in a fixed zone and build noon directly, rather than parsing
	// midnight in the source zone and adding twelve hours. Both shortcuts are wrong around a
	// daylight saving transition: midnight does not exist at all in zones that spring forward
	// at 00:00 (Santiago, Havana, Beirut, Cairo, Tehran), and adding a wall-clock twelve hours
	// across a transition lands on 11:00 or 13:00 instead of noon, which for far-apart zone
	// pairs changes the resulting date.
	parsed, errParse := time.Parse(codexEnvironmentDateLayout, date)
	if errParse != nil {
		return "", false
	}
	year, month, day := parsed.Date()
	noon := time.Date(year, month, day, 12, 0, 0, 0, source)
	return noon.In(target).Format(codexEnvironmentDateLayout), true
}

// readCodexEnvironmentTag returns the trimmed value of one simple tag, or "" when the tag is
// absent or empty.
func readCodexEnvironmentTag(block string, tag string) string {
	start, end, ok := codexEnvironmentTagBounds(block, tag)
	if !ok {
		return ""
	}
	return strings.TrimSpace(block[start:end])
}

// replaceCodexEnvironmentTag swaps the value of one simple tag. A missing tag is left alone:
// the rewrite never invents fields the client did not send. An empty tag is left alone too —
// the client emitting no value means it could not determine one, and filling it in would be
// fabricating data rather than correcting it.
func replaceCodexEnvironmentTag(block string, tag string, value string) string {
	start, end, ok := codexEnvironmentTagBounds(block, tag)
	if !ok || strings.TrimSpace(block[start:end]) == "" || block[start:end] == value {
		return block
	}
	return block[:start] + value + block[end:]
}

// codexEnvironmentTagBounds locates the value span of the first occurrence of a tag.
func codexEnvironmentTagBounds(block string, tag string) (int, int, bool) {
	openTag := "<" + tag + ">"
	closeTag := "</" + tag + ">"
	start := strings.Index(block, openTag)
	if start < 0 {
		return 0, 0, false
	}
	valueStart := start + len(openTag)
	relativeEnd := strings.Index(block[valueStart:], closeTag)
	if relativeEnd < 0 {
		return 0, 0, false
	}
	return valueStart, valueStart + relativeEnd, true
}
