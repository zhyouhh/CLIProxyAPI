package auth

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"testing"
	"time"
)

func TestVerifiedCodexRecoveryPreservesOtherFailuresAndGeneration(t *testing.T) {
	m := NewManager(nil, nil, nil)
	next := time.Now().Add(100 * time.Hour)
	a := &Auth{ID: "recovery", Provider: "codex", Status: StatusError, ModelStates: map[string]*ModelState{
		"gpt-test": {Status: StatusError, Unavailable: true, NextRetryAfter: next, Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next}, LastError: &Error{HTTPStatus: 429, Message: `{"error":{"type":"usage_limit_reached"}}`}},
		"gpt-bad":  {Status: StatusError, Unavailable: true, NextRetryAfter: next, LastError: &Error{HTTPStatus: 401, Message: "expired"}},
	}}
	m.Register(context.Background(), a)
	old, _ := m.GetByID(a.ID)
	_, _, err := m.RecoverCodexQuota(context.Background(), a.ID, old.RegistrationEpoch, old.Generation+1)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetByID(a.ID)
	if !got.ModelStates["gpt-test"].Unavailable {
		t.Fatal("stale observation cleared state")
	}
	_, _, err = m.RecoverCodexQuota(context.Background(), a.ID, old.RegistrationEpoch, old.Generation)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = m.GetByID(a.ID)
	if got.ModelStates["gpt-test"].Unavailable {
		t.Fatal("verified quota still blocked")
	}
	if !got.ModelStates["gpt-bad"].Unavailable || got.ModelStates["gpt-bad"].LastError.HTTPStatus != 401 {
		t.Fatal("unrelated failure cleared")
	}
}

func TestVerifiedCodexRecoveryRestoresRegistryAndScheduler(t *testing.T) {
	m := NewManager(nil, nil, nil)
	ctx := context.Background()
	id, model := "verified-codex-routing", "gpt-recovery"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(id) })
	next := time.Now().Add(112 * time.Hour)
	a := &Auth{ID: id, Provider: "codex", Status: StatusError, Unavailable: true, NextRetryAfter: next,
		Quota:     QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
		LastError: &Error{HTTPStatus: 429, Message: `{"error":{"type":"usage_limit_reached"}}`},
		ModelStates: map[string]*ModelState{model: {Status: StatusError, Unavailable: true, NextRetryAfter: next,
			Quota:     QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
			LastError: &Error{HTTPStatus: 429, Message: `{"error":{"type":"usage_limit_reached"}}`}}}}
	if _, err := m.Register(ctx, a); err != nil {
		t.Fatal(err)
	}
	reg.SetModelQuotaExceeded(id, model)
	reg.SuspendClientModel(id, model, "quota")
	before, _ := m.GetByID(id)
	updated, models, err := m.RecoverCodexQuota(ctx, id, before.RegistrationEpoch, before.Generation)
	if err != nil || updated == nil || len(models) != 1 {
		t.Fatalf("recovery: %v %v", models, err)
	}
	if reg.GetModelCount(model) != 1 {
		t.Fatal("registry remains blocked")
	}
	if updated.Unavailable || updated.Quota.Exceeded || !updated.NextRetryAfter.IsZero() {
		t.Fatal("auth still blocked")
	}
	picked, err := m.scheduler.pickSingle(ctx, "codex", model, cliproxyexecutor.Options{}, nil)
	if err != nil || picked == nil {
		t.Fatalf("scheduler still blocked: %v", err)
	}
}

func TestVerifiedCodexRecoveryPreservesAuthLevel401(t *testing.T) {
	m := NewManager(nil, nil, nil)
	ctx := context.Background()
	next := time.Now().Add(time.Hour)
	a := &Auth{ID: "auth-401", Provider: "codex", Status: StatusError, Unavailable: true, NextRetryAfter: next,
		LastError: &Error{HTTPStatus: 401, Message: "refresh failed"}, ModelStates: map[string]*ModelState{
			"gpt-test": {Unavailable: true, Status: StatusError, NextRetryAfter: next,
				Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next}, LastError: &Error{HTTPStatus: 429, Message: `{"error":{"type":"usage_limit_reached"}}`}}}}
	m.Register(ctx, a)
	old, _ := m.GetByID(a.ID)
	m.RecoverCodexQuota(ctx, a.ID, old.RegistrationEpoch, old.Generation)
	got, _ := m.GetByID(a.ID)
	if got.LastError == nil || got.LastError.HTTPStatus != 401 || !got.Unavailable {
		t.Fatal("auth 401 erased")
	}
}

func TestCodexRecoveryAcceptsExecutorErrorShapes(t *testing.T) {
	q := QuotaState{Exceeded: true, Reason: "quota"}
	for _, body := range []string{`{"type":"usage_limit_reached"}`, `{"error":{"type":" USAGE_LIMIT_REACHED "}}`} {
		if !recoverableCodexQuota(q, &Error{HTTPStatus: 429, Message: body}) {
			t.Fatal("executor quota cannot recover", body)
		}
	}
}

func TestVerifiedCodexRecoveryRejectsReRegisteredCredential(t *testing.T) {
	m := NewManager(nil, nil, nil)
	ctx := context.Background()
	a := &Auth{ID: "same-id", Provider: "codex", Status: StatusActive}
	m.Register(ctx, a)
	old, _ := m.GetByID(a.ID)
	next := time.Now().Add(time.Hour)
	replacement := &Auth{ID: a.ID, Provider: "codex", Status: StatusError, ModelStates: map[string]*ModelState{
		"gpt-test": {Status: StatusError, Unavailable: true, NextRetryAfter: next, Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next}, LastError: &Error{HTTPStatus: 429, Message: `{"error":{"type":"usage_limit_reached"}}`}}}}
	m.Register(ctx, replacement)
	current, _ := m.GetByID(a.ID)
	if current.RegistrationEpoch == old.RegistrationEpoch {
		t.Fatal("fixture did not re-register")
	}
	// The old and replacement generation may coincide; epoch must distinguish them.
	m.RecoverCodexQuota(ctx, a.ID, old.RegistrationEpoch, current.Generation)
	got, _ := m.GetByID(a.ID)
	if !got.ModelStates["gpt-test"].Unavailable {
		t.Fatal("old credential probe unlocked replacement")
	}
}

func TestVerifiedCodexRecoveryIgnoresResolvedSiblingErrorSummary(t *testing.T) {
	m := NewManager(nil, nil, nil)
	ctx := context.Background()
	next := time.Now().Add(time.Hour)
	a := &Auth{ID: "resolved-summary", Provider: "codex", Status: StatusError, LastError: &Error{HTTPStatus: 503, Message: "old B failure"},
		ModelStates: map[string]*ModelState{
			"A": {Status: StatusError, Unavailable: true, NextRetryAfter: next, Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next}, LastError: &Error{HTTPStatus: 429, Message: `{"error":{"type":"usage_limit_reached"}}`}},
			"B": {Status: StatusActive}}}
	m.Register(ctx, a)
	old, _ := m.GetByID(a.ID)
	m.RecoverCodexQuota(ctx, a.ID, old.RegistrationEpoch, old.Generation)
	got, _ := m.GetByID(a.ID)
	if got.ModelStates["A"].Unavailable {
		t.Fatal("resolved B summary prevents A recovery")
	}
}
