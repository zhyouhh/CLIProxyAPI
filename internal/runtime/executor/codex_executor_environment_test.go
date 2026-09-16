package executor

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/tidwall/gjson"
)

// codexEnvironmentContextSample reproduces the shape observed in real Codex CLI and Codex
// Desktop rollouts: the whole user message is one environment context block.
const codexEnvironmentContextSample = `<environment_context>
  <cwd>/Users/someone/project</cwd>
  <shell>zsh</shell>
  <current_date>2026-09-16</current_date>
  <timezone>Asia/Shanghai</timezone>
  <filesystem><workspace_roots><root>/Users/someone/project</root></workspace_roots></filesystem>
</environment_context>`

// codexEnvironmentPayload builds a request body shaped like the one Codex clients send.
func codexEnvironmentPayload(environmentTexts ...string) []byte {
	items := []string{`{"role":"developer","type":"message","content":[{"type":"input_text","text":"You are Codex."}]}`}
	for _, text := range environmentTexts {
		items = append(items, `{"role":"user","type":"message","content":[{"type":"input_text","text":`+quoteJSON(text)+`}]}`)
	}
	items = append(items, `{"role":"user","type":"message","content":[{"type":"input_text","text":"draw a pelican"}]}`)
	return []byte(`{"model":"gpt-6-astra","input":[` + strings.Join(items, ",") + `],"prompt_cache_key":"keep-me"}`)
}

func quoteJSON(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + replacer.Replace(value) + `"`
}

func environmentTextAt(t *testing.T, payload []byte, index int) string {
	t.Helper()
	return gjson.GetBytes(payload, "input."+strconv.Itoa(index)+".content.0.text").String()
}

func losAngeles(t *testing.T) *time.Location {
	t.Helper()
	location, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load America/Los_Angeles: %v", err)
	}
	return location
}

func TestApplyCodexEnvironmentContextDisabledByDefault(t *testing.T) {
	payload := codexEnvironmentPayload(codexEnvironmentContextSample)
	got := applyCodexEnvironmentContext(&config.Config{}, &cliproxyauth.Auth{ID: "codex-1"}, payload)
	if string(got) != string(payload) {
		t.Fatalf("expected payload untouched when no timezone is configured, got %s", got)
	}
}

func TestApplyCodexEnvironmentContextRewritesTimezoneAndShiftsDate(t *testing.T) {
	cfg := &config.Config{}
	cfg.CodexHeaderDefaults.Timezone = "America/Los_Angeles"

	got := applyCodexEnvironmentContext(cfg, &cliproxyauth.Auth{ID: "codex-1"}, codexEnvironmentPayload(codexEnvironmentContextSample))

	text := environmentTextAt(t, got, 1)
	if !strings.Contains(text, "<timezone>America/Los_Angeles</timezone>") {
		t.Fatalf("timezone not rewritten: %s", text)
	}
	if strings.Contains(text, "Asia/Shanghai") {
		t.Fatalf("original timezone still present: %s", text)
	}
	// Noon of 2026-09-16 in Shanghai is 2026-09-15 21:00 in Los Angeles.
	if !strings.Contains(text, "<current_date>2026-09-15</current_date>") {
		t.Fatalf("date not shifted into the target timezone: %s", text)
	}
	if !strings.Contains(text, "<cwd>/Users/someone/project</cwd>") || !strings.Contains(text, "<shell>zsh</shell>") {
		t.Fatalf("unrelated environment fields were modified: %s", text)
	}
	if gjson.GetBytes(got, "prompt_cache_key").String() != "keep-me" {
		t.Fatalf("unrelated payload fields were modified: %s", got)
	}
	if environmentTextAt(t, got, 2) != "draw a pelican" {
		t.Fatalf("unrelated input items were modified: %s", got)
	}
}

// TestRewriteCodexEnvironmentContextIsIndependentOfWallClock pins the property the whole date
// design exists for: the rewrite is a pure function of the payload, so a long session never
// sees its cached upstream prefix change underneath it.
func TestRewriteCodexEnvironmentContextIsIndependentOfWallClock(t *testing.T) {
	payload := codexEnvironmentPayload(codexEnvironmentContextSample)

	first, _ := rewriteCodexEnvironmentContext(payload, "America/Los_Angeles", losAngeles(t))
	second, _ := rewriteCodexEnvironmentContext(payload, "America/Los_Angeles", losAngeles(t))
	if string(first) != string(second) {
		t.Fatalf("rewrite is not deterministic:\n%s\n%s", first, second)
	}
	// Use a date that can never be today, so the assertion below cannot short-circuit into
	// passing vacuously on the day the fixture happens to match the wall clock.
	dated := strings.Replace(codexEnvironmentContextSample, "2026-09-16", "2001-02-03", 1)
	shifted, _ := rewriteCodexEnvironmentContext(codexEnvironmentPayload(dated), "America/Los_Angeles", losAngeles(t))
	text := environmentTextAt(t, shifted, 1)
	if !strings.Contains(text, "<current_date>2001-02-02</current_date>") {
		t.Fatalf("date was not shifted from the payload's own value: %s", text)
	}
	if strings.Contains(text, time.Now().Format("2006-01-02")) {
		t.Fatalf("rewrite leaked the current date into the payload: %s", text)
	}
}

// TestRewriteCodexEnvironmentContextKeepsHistoricalDatesApart is the regression test for
// collapsing every block onto today: each block shifts from its own original date.
func TestRewriteCodexEnvironmentContextKeepsHistoricalDatesApart(t *testing.T) {
	older := strings.Replace(codexEnvironmentContextSample, "2026-09-16", "2026-09-13", 1)
	payload := codexEnvironmentPayload(older, codexEnvironmentContextSample)

	got, matched := rewriteCodexEnvironmentContext(payload, "America/Los_Angeles", losAngeles(t))
	if !matched {
		t.Fatal("expected the environment blocks to be recognised")
	}
	if want := "<current_date>2026-09-12</current_date>"; !strings.Contains(environmentTextAt(t, got, 1), want) {
		t.Fatalf("historical block lost its own date: %s", environmentTextAt(t, got, 1))
	}
	if want := "<current_date>2026-09-15</current_date>"; !strings.Contains(environmentTextAt(t, got, 2), want) {
		t.Fatalf("live block not shifted: %s", environmentTextAt(t, got, 2))
	}
	for _, index := range []int{1, 2} {
		if !strings.Contains(environmentTextAt(t, got, index), "<timezone>America/Los_Angeles</timezone>") {
			t.Fatalf("block %d kept the client timezone: %s", index, environmentTextAt(t, got, index))
		}
	}
}

// TestRewriteCodexEnvironmentContextIgnoresQuotedBlocks is the regression test for silently
// editing conversation content that merely quotes an environment context, such as a pasted
// session log.
func TestRewriteCodexEnvironmentContextIgnoresQuotedBlocks(t *testing.T) {
	quoted := "here is what my old session sent:\n" + codexEnvironmentContextSample + "\nwhy is the timezone wrong?"
	payload := codexEnvironmentPayload(quoted)

	got, matched := rewriteCodexEnvironmentContext(payload, "America/Los_Angeles", losAngeles(t))
	if matched {
		t.Fatal("a quoted block must not count as an environment context item")
	}
	if string(got) != string(payload) {
		t.Fatalf("quoted block was rewritten: %s", environmentTextAt(t, got, 1))
	}
}

func TestRewriteCodexEnvironmentContextLeavesForeignPayloadsAlone(t *testing.T) {
	cases := map[string][]byte{
		"no environment context": codexEnvironmentPayload("just a prompt"),
		"input is not an array":  []byte(`{"input":"just a prompt"}`),
		"no input at all":        []byte(`{"model":"gpt-6-astra"}`),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			got, matched := rewriteCodexEnvironmentContext(payload, "America/Los_Angeles", losAngeles(t))
			if matched {
				t.Fatal("nothing should have matched")
			}
			if string(got) != string(payload) {
				t.Fatalf("payload was modified, got %s", got)
			}
		})
	}
}

func TestRewriteCodexEnvironmentContextStringContentShape(t *testing.T) {
	payload := []byte(`{"input":[{"role":"user","content":` + quoteJSON(codexEnvironmentContextSample) + `}]}`)

	got, matched := rewriteCodexEnvironmentContext(payload, "America/Los_Angeles", losAngeles(t))
	if !matched {
		t.Fatal("string content shape was not recognised")
	}
	text := gjson.GetBytes(got, "input.0.content").String()
	if !strings.Contains(text, "<timezone>America/Los_Angeles</timezone>") || !strings.Contains(text, "<current_date>2026-09-15</current_date>") {
		t.Fatalf("string content shape not rewritten: %s", text)
	}
}

func TestRewriteCodexEnvironmentContextNeverInventsOrFillsTags(t *testing.T) {
	cases := map[string]struct {
		block   string
		absent  string
		present string
	}{
		"missing current_date": {
			block:   "<environment_context>\n  <shell>zsh</shell>\n  <timezone>Asia/Shanghai</timezone>\n</environment_context>",
			absent:  "current_date",
			present: "<timezone>America/Los_Angeles</timezone>",
		},
		"empty timezone stays empty": {
			block:   "<environment_context>\n  <current_date>2026-09-16</current_date>\n  <timezone></timezone>\n</environment_context>",
			absent:  "America/Los_Angeles",
			present: "<timezone></timezone>",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			got, matched := rewriteCodexEnvironmentContext(codexEnvironmentPayload(testCase.block), "America/Los_Angeles", losAngeles(t))
			if !matched {
				t.Fatal("block should still be recognised")
			}
			text := environmentTextAt(t, got, 1)
			if strings.Contains(text, testCase.absent) {
				t.Fatalf("value was fabricated (%q present): %s", testCase.absent, text)
			}
			if !strings.Contains(text, testCase.present) {
				t.Fatalf("expected %q to survive: %s", testCase.present, text)
			}
		})
	}
}

func TestShiftCodexEnvironmentDateDegradesToNoOp(t *testing.T) {
	cases := map[string][2]string{
		"unknown source timezone": {"2026-09-16", "Not/AZone"},
		"unparsable date":         {"tomorrow", "Asia/Shanghai"},
		"missing date":            {"", "Asia/Shanghai"},
		"missing source timezone": {"2026-09-16", ""},
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := shiftCodexEnvironmentDate(input[0], input[1], losAngeles(t)); ok {
				t.Fatal("expected the shift to decline rather than guess")
			}
		})
	}
}

func TestApplyCodexEnvironmentContextInvalidTimezoneIsNoOp(t *testing.T) {
	cfg := &config.Config{}
	cfg.CodexHeaderDefaults.Timezone = "Not/AZone"

	payload := codexEnvironmentPayload(codexEnvironmentContextSample)
	got := applyCodexEnvironmentContext(cfg, &cliproxyauth.Auth{ID: "codex-1"}, payload)
	if string(got) != string(payload) {
		t.Fatalf("an unparsable timezone must leave the payload untouched, got %s", got)
	}
}

func TestApplyCodexEnvironmentContextCredentialOverridesGlobal(t *testing.T) {
	cfg := &config.Config{}
	cfg.CodexHeaderDefaults.Timezone = "America/Los_Angeles"

	cases := map[string]*cliproxyauth.Auth{
		"attribute": {ID: "codex-1", Attributes: map[string]string{"timezone": "Europe/Berlin"}},
		"metadata":  {ID: "codex-1", Metadata: map[string]any{"timezone": "Europe/Berlin"}},
		"alias":     {ID: "codex-1", Metadata: map[string]any{"time_zone": "Europe/Berlin"}},
	}
	for name, auth := range cases {
		t.Run(name, func(t *testing.T) {
			got := applyCodexEnvironmentContext(cfg, auth, codexEnvironmentPayload(codexEnvironmentContextSample))
			if !strings.Contains(environmentTextAt(t, got, 1), "<timezone>Europe/Berlin</timezone>") {
				t.Fatalf("credential timezone did not win over the global default: %s", environmentTextAt(t, got, 1))
			}
		})
	}
}

// TestApplyCodexUpstreamBodyRewritesKeepsGuardsIndependent pins that turning identity
// confusion off does not silently turn the environment rewrite off with it.
func TestApplyCodexUpstreamBodyRewritesKeepsGuardsIndependent(t *testing.T) {
	cfg := &config.Config{}
	cfg.CodexHeaderDefaults.Timezone = "America/Los_Angeles"
	cfg.Codex.IdentityConfuse = false

	payload := codexEnvironmentPayload(codexEnvironmentContextSample)
	got, state := applyCodexUpstreamBodyRewrites(cfg, &cliproxyauth.Auth{ID: "codex-1"}, payload, payload)

	if state.enabled {
		t.Fatal("identity confusion should stay disabled")
	}
	if !strings.Contains(environmentTextAt(t, got, 1), "<timezone>America/Los_Angeles</timezone>") {
		t.Fatalf("environment rewrite was suppressed by the identity confusion guard: %s", environmentTextAt(t, got, 1))
	}
}

// TestApplyCodexUpstreamBodyRewritesLeavesCallerBufferIntact pins the websocket contract that
// the downstream client keeps seeing its own body while only the upstream copy is rewritten.
func TestApplyCodexUpstreamBodyRewritesLeavesCallerBufferIntact(t *testing.T) {
	cfg := &config.Config{}
	cfg.CodexHeaderDefaults.Timezone = "America/Los_Angeles"

	clientBody := codexEnvironmentPayload(codexEnvironmentContextSample)
	original := string(clientBody)

	upstreamBody, _ := applyCodexUpstreamBodyRewrites(cfg, &cliproxyauth.Auth{ID: "codex-1"}, clientBody, clientBody)

	if string(clientBody) != original {
		t.Fatalf("the caller's buffer was mutated: %s", clientBody)
	}
	if string(upstreamBody) == original {
		t.Fatal("the upstream copy was not rewritten")
	}
}

// TestCodexUpstreamBodiesAllGoThroughTheMergedRewrite is the regression test for the class of
// bug where one upstream path remembers a rewrite and another forgets it: every caller must
// go through applyCodexUpstreamBodyRewrites rather than the identity rewrite alone.
func TestCodexUpstreamBodiesAllGoThroughTheMergedRewrite(t *testing.T) {
	const merged = "applyCodexUpstreamBodyRewrites"
	const identityOnly = "applyCodexIdentityConfuseBody"

	fileSet := token.NewFileSet()
	packages, errParse := parser.ParseDir(fileSet, ".", func(info os.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if errParse != nil {
		t.Fatalf("parse package: %v", errParse)
	}

	mergedCallSites := 0
	for _, pkg := range packages {
		for path, file := range pkg.Files {
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				ident, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				switch ident.Name {
				case merged:
					mergedCallSites++
				case identityOnly:
					// The merged helper is the single legitimate caller.
					if filepath.Base(path) != "codex_executor_request.go" {
						t.Errorf("%s calls %s directly; it must call %s so the environment rewrite cannot be forgotten",
							filepath.Base(path), identityOnly, merged)
					}
				}
				return true
			})
		}
	}
	if mergedCallSites < 3 {
		t.Fatalf("expected the HTTP, websocket and websocket stream paths to call %s, found %d call sites", merged, mergedCallSites)
	}
}

// TestCodexExecutorCacheHelperRewritesEnvironmentContext covers the HTTP/SSE wiring: the
// rewritten body must reach the upstream request, not just the return value.
func TestCodexExecutorCacheHelperRewritesEnvironmentContext(t *testing.T) {
	cfg := &config.Config{}
	cfg.CodexHeaderDefaults.Timezone = "America/Los_Angeles"
	executor := NewCodexExecutor(cfg)

	rawJSON := codexEnvironmentPayload(codexEnvironmentContextSample)
	req := cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: rawJSON}

	httpReq, body, _, err := executor.cacheHelper(
		context.Background(),
		sdktranslator.FromString("openai"),
		"https://example.com/responses",
		&cliproxyauth.Auth{ID: "codex-1"},
		req,
		req.Payload,
		rawJSON,
	)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}
	if !strings.Contains(environmentTextAt(t, body, 1), "<timezone>America/Los_Angeles</timezone>") {
		t.Fatalf("returned body was not rewritten: %s", body)
	}
	sent, errRead := io.ReadAll(httpReq.Body)
	if errRead != nil {
		t.Fatalf("read request body: %v", errRead)
	}
	if !strings.Contains(environmentTextAt(t, sent, 1), "<timezone>America/Los_Angeles</timezone>") {
		t.Fatalf("upstream request body was not rewritten: %s", sent)
	}
}

// TestRewriteCodexEnvironmentContextAgainstRealCapture runs the rewrite against a payload
// captured from a real Codex Desktop session resumed through a local recording endpoint, with
// only the filesystem paths anonymised. A hand-written fixture cannot show that the admission
// condition still matches what the client actually puts on the wire; this one can, and it
// carries the two-block shape (one historical turn, one live) that resumed sessions produce.
func TestRewriteCodexEnvironmentContextAgainstRealCapture(t *testing.T) {
	payload, errRead := os.ReadFile(filepath.Join("testdata", "codex_environment_context_capture.json"))
	if errRead != nil {
		t.Fatalf("read captured payload: %v", errRead)
	}

	got, matched := rewriteCodexEnvironmentContext(payload, "America/Los_Angeles", losAngeles(t))
	if !matched {
		t.Fatal("the admission condition no longer matches a real captured payload")
	}

	blocks := 0
	dates := map[string]int{}
	for _, item := range gjson.GetBytes(got, "input").Array() {
		for _, part := range item.Get("content").Array() {
			text := part.Get("text").String()
			if !strings.Contains(text, codexEnvironmentContextOpen) {
				continue
			}
			blocks++
			if !strings.Contains(text, "<timezone>America/Los_Angeles</timezone>") {
				t.Fatalf("captured block kept the client timezone: %s", text)
			}
			if strings.Contains(text, "Asia/Shanghai") {
				t.Fatalf("captured block still leaks the client timezone: %s", text)
			}
			dates[readCodexEnvironmentTag(text, "current_date")]++
		}
	}
	if blocks != 2 {
		t.Fatalf("expected the captured two-block shape, found %d blocks", blocks)
	}
	// 2026-08-30 and 2026-09-16 in Shanghai shift to the day before in Los Angeles and must
	// stay distinct rather than collapsing onto one date.
	if dates["2026-08-29"] != 1 || dates["2026-09-15"] != 1 {
		t.Fatalf("captured blocks did not keep their own shifted dates: %v", dates)
	}
}

// TestApplyCodexEnvironmentContextWarnsWhenNothingMatches pins the tripwire: if a future client
// stops sending a recognisable environment context, requests quietly fall back to reporting the
// operator's own timezone, so that has to show up in the log rather than pass silently.
func TestApplyCodexEnvironmentContextWarnsWhenNothingMatches(t *testing.T) {
	codexEnvironmentMissLoggedAt.Store(0)
	defer codexEnvironmentMissLoggedAt.Store(0)

	hook := test.NewGlobal()
	defer hook.Reset()

	cfg := &config.Config{}
	cfg.CodexHeaderDefaults.Timezone = "America/Los_Angeles"
	applyCodexEnvironmentContext(cfg, &cliproxyauth.Auth{ID: "codex-1"}, codexEnvironmentPayload("no environment here"))

	warned := false
	for _, entry := range hook.AllEntries() {
		if entry.Level == logrus.WarnLevel && strings.Contains(entry.Message, "no <environment_context> input item matched") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("expected a warning when the rewrite is configured but matches nothing, got %d entries", len(hook.AllEntries()))
	}

	// The warning is throttled so it cannot fire once per request.
	hook.Reset()
	applyCodexEnvironmentContext(cfg, &cliproxyauth.Auth{ID: "codex-1"}, codexEnvironmentPayload("no environment here"))
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, "no <environment_context> input item matched") {
			t.Fatal("the warning is not throttled")
		}
	}
}

// TestRewriteCodexEnvironmentContextRewritesEveryBlockInOneText is the regression test for a
// text that holds several concatenated blocks: rewriting only the first would send the client's
// real timezone upstream alongside the rewritten one, in the same request.
func TestRewriteCodexEnvironmentContextRewritesEveryBlockInOneText(t *testing.T) {
	older := strings.Replace(codexEnvironmentContextSample, "2026-09-16", "2026-09-13", 1)
	doubled := codexEnvironmentContextSample + "\n" + older

	got, matched := rewriteCodexEnvironmentContext(codexEnvironmentPayload(doubled), "America/Los_Angeles", losAngeles(t))
	if !matched {
		t.Fatal("expected the concatenated blocks to be recognised")
	}
	text := environmentTextAt(t, got, 1)
	if strings.Contains(text, "Asia/Shanghai") {
		t.Fatalf("a later block leaked the client timezone upstream: %s", text)
	}
	if strings.Count(text, "<timezone>America/Los_Angeles</timezone>") != 2 {
		t.Fatalf("expected both blocks rewritten: %s", text)
	}
	if !strings.Contains(text, "<current_date>2026-09-15</current_date>") || !strings.Contains(text, "<current_date>2026-09-12</current_date>") {
		t.Fatalf("each block should keep its own shifted date: %s", text)
	}
}

// TestShiftCodexEnvironmentDateSurvivesDaylightSavingEdges pins the anchor against the two
// wall-clock traps: zones where local midnight does not exist on a transition day, and pairs
// far enough apart that an anchor landing at 11:00 or 13:00 instead of noon changes the date.
func TestShiftCodexEnvironmentDateSurvivesDaylightSavingEdges(t *testing.T) {
	cases := []struct {
		date   string
		source string
		target string
		want   string
	}{
		// Local midnight does not exist on these dates in these zones.
		{"2025-09-07", "America/Santiago", "America/Los_Angeles", "2025-09-07"},
		{"2025-03-09", "America/Havana", "America/Los_Angeles", "2025-03-09"},
		{"2025-03-30", "Asia/Beirut", "America/Los_Angeles", "2025-03-30"},
		{"2025-04-25", "Africa/Cairo", "America/Los_Angeles", "2025-04-25"},
		// Far-apart pairs across a transition: noon must stay noon or the date slips.
		{"2025-03-30", "Europe/Berlin", "Pacific/Auckland", "2025-03-30"},
		{"2025-10-26", "Europe/Berlin", "Pacific/Auckland", "2025-10-27"},
		{"2026-03-29", "Europe/Berlin", "Pacific/Auckland", "2026-03-29"},
		// The deployment case, on an ordinary day and on a US transition day.
		{"2026-09-16", "Asia/Shanghai", "America/Los_Angeles", "2026-09-15"},
		{"2026-11-01", "Asia/Shanghai", "America/Los_Angeles", "2026-10-31"},
	}
	for _, testCase := range cases {
		t.Run(testCase.source+"->"+testCase.target+"@"+testCase.date, func(t *testing.T) {
			target, errLocation := time.LoadLocation(testCase.target)
			if errLocation != nil {
				t.Fatalf("load %s: %v", testCase.target, errLocation)
			}
			got, ok := shiftCodexEnvironmentDate(testCase.date, testCase.source, target)
			if !ok {
				t.Fatal("shift declined on a valid date")
			}
			if got != testCase.want {
				t.Fatalf("shift = %s, want %s", got, testCase.want)
			}
		})
	}
}

// TestLogCodexEnvironmentMissSurvivesClockRollback pins that an NTP correction cannot mute the
// tripwire: a stamp from the future would otherwise keep the warning suppressed until real time
// caught up with it.
func TestLogCodexEnvironmentMissSurvivesClockRollback(t *testing.T) {
	codexEnvironmentMissLoggedAt.Store(time.Now().Add(time.Hour).UnixNano())
	defer codexEnvironmentMissLoggedAt.Store(0)

	hook := test.NewGlobal()
	defer hook.Reset()

	logCodexEnvironmentMiss("America/Los_Angeles")

	for _, entry := range hook.AllEntries() {
		if entry.Level == logrus.WarnLevel && strings.Contains(entry.Message, "no <environment_context> input item matched") {
			return
		}
	}
	t.Fatal("a backwards clock jump silenced the tripwire")
}

// TestRewriteCodexEnvironmentContextKeepsClientEscaping pins that the rewrite does not invent a
// fingerprint of its own. The Rust client writes '<' literally; sjson would encode it as
// <, leaving one request carrying two different escapings of the same character, which no
// real client produces.
func TestRewriteCodexEnvironmentContextKeepsClientEscaping(t *testing.T) {
	payload := codexEnvironmentPayload(codexEnvironmentContextSample)
	escapedLT := []byte("\\u003c")
	if bytes.Contains(payload, escapedLT) {
		t.Fatal("fixture already uses escaped angle brackets; the test would be vacuous")
	}

	got, matched := rewriteCodexEnvironmentContext(payload, "America/Los_Angeles", losAngeles(t))
	if !matched {
		t.Fatal("expected the block to be rewritten")
	}
	if bytes.Contains(got, escapedLT) || bytes.Contains(got, []byte("\\u003e")) || bytes.Contains(got, []byte("\\u0026")) {
		t.Fatalf("rewrite introduced escaped angle brackets the client never sends: %s", got)
	}
	if !bytes.Contains(got, []byte(`<environment_context>`)) {
		t.Fatalf("rewritten block lost its literal delimiters: %s", got)
	}
}

// TestRewriteCodexEnvironmentContextSurvivesNestedDelimiter is the regression test for a block
// whose own content names the closing delimiter: pairing on the first one would cut the block
// short and silently leave the real timezone and date behind, with matched still true so the
// tripwire stays quiet.
func TestRewriteCodexEnvironmentContextSurvivesNestedDelimiter(t *testing.T) {
	block := "<environment_context>\n" +
		"  <cwd>/Users/someone/project</cwd>\n" +
		"  <subagents>\n    - doc_review: explains <environment_context></environment_context> handling\n  </subagents>\n" +
		"  <current_date>2026-09-16</current_date>\n" +
		"  <timezone>Asia/Shanghai</timezone>\n" +
		"</environment_context>"

	got, matched := rewriteCodexEnvironmentContext(codexEnvironmentPayload(block), "America/Los_Angeles", losAngeles(t))
	if !matched {
		t.Fatal("expected the block to be recognised")
	}
	text := environmentTextAt(t, got, 1)
	if !strings.Contains(text, "<current_date>2026-09-15</current_date>") {
		t.Fatalf("a nested closing delimiter hid the real current_date: %s", text)
	}
	if strings.Contains(text, "<timezone>Asia/Shanghai</timezone>") {
		t.Fatalf("a nested closing delimiter hid the real timezone: %s", text)
	}
	if !strings.Contains(text, "doc_review: explains <environment_context></environment_context> handling") {
		t.Fatalf("the nested text itself was damaged: %s", text)
	}
}
