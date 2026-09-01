package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/spawn08/chronos-code/internal/auth"
	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/orchestrator"
)

// newTestModel builds a minimal, real Orchestrator (no live model calls are
// made in these tests — only slash-command handlers that touch
// config/auth/model-registry state) and wraps it in an appModel, the same
// shape RunTUI builds, so these tests exercise the actual TUI code path
// rather than a stand-in.
//
// It isolates $HOME so file-based auth state (~/.chronos-code,
// ~/.claude, ~/.codex reuse) never touches the real host. This
// deliberately also breaks the real OS keychain, since go-keyring's macOS
// backend shells out to `security`, which resolves the login keychain via
// $HOME — verified directly: keyring.Set succeeds against the real $HOME
// and fails with "exit status 154" under an overridden one. Tests that
// need the real keychain to actually succeed (not just be reachable) use
// newTestModelRealHome instead, with a disposable, cleaned-up provider
// name so they never collide with the host's real stored credentials.
func newTestModel(t *testing.T) *appModel {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // isolate from any real ~/.chronos-code or ~/.claude on the host.
	return buildTestModel(t)
}

// newTestModelRealHome is newTestModel without the $HOME override, for
// tests that must exercise a real (not isolated-away) OS keychain
// round-trip. Callers are responsible for using a disposable provider name
// and cleaning it up (see TestHandleLoginCommandAPIKeyThenWhoami).
func newTestModelRealHome(t *testing.T) *appModel {
	t.Helper()
	return buildTestModel(t)
}

func buildTestModel(t *testing.T) *appModel {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgYAML := `
defaults:
  model:
    provider: anthropic
    model: claude-sonnet-4-6
  storage:
    backend: sqlite
    dsn: ":memory:"
agents:
  - id: coder
    name: Coder
    model:
      provider: anthropic
      model: claude-sonnet-4-6
`
	cfgPath := filepath.Join(root, "chronos-code.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	oldWd, _ := os.Getwd()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(oldWd) })

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	orch, err := orchestrator.New(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}
	t.Cleanup(func() { orch.Close() })

	m := &appModel{
		orch:     orch,
		ctx:      context.Background(),
		input:    textarea.New(),
		spin:     spinner.New(),
		history:  NewHistory(),
		viewport: viewport.New(80, 24),
	}
	m.viewport.Width = 80
	return m
}

func lastBlock(m *appModel) string {
	if len(m.blocks) == 0 {
		return ""
	}
	return m.blocks[len(m.blocks)-1]
}

func TestHandleModelCommandShowsNoCatalogWhenNothingAuthorized(t *testing.T) {
	m := newTestModel(t) // isolated $HOME/env: no provider is authorized here.
	m.handleModelCommand("")
	out := lastBlock(m)
	if !strings.Contains(out, "active: anthropic / claude-sonnet-4-6") {
		t.Fatalf("output = %q, want active model line", out)
	}
	if !strings.Contains(out, "context window: 200.0k tokens") {
		t.Fatalf("output = %q, want context window for a known model", out)
	}
	if !strings.Contains(out, "no provider is authorized yet") {
		t.Fatalf("output = %q, want it to say nothing is authorized rather than dumping the full static catalog", out)
	}
	if strings.Contains(out, "gemini") || strings.Contains(out, "openai") {
		t.Fatalf("output = %q, want no unauthorized providers listed at all", out)
	}
}

func TestHandleModelCommandListsOnlyAuthorizedProvidersWhenSomeAreConfigured(t *testing.T) {
	m := newTestModel(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-env-key") // authorizes anthropic only.
	m.handleModelCommand("")
	out := lastBlock(m)
	if !strings.Contains(out, "models (static registry, authorized providers only)") {
		t.Fatalf("output = %q, want the authorized-providers-only label", out)
	}
	if !strings.Contains(out, "anthropic") {
		t.Fatalf("output = %q, want anthropic listed (it's authorized)", out)
	}
	if strings.Contains(out, "openai") || strings.Contains(out, "gemini") || strings.Contains(out, "mistral") {
		t.Fatalf("output = %q, want no unauthorized providers listed", out)
	}
}

func TestHandleModelCommandSwitchesModel(t *testing.T) {
	m := newTestModel(t)
	m.handleModelCommand("anthropic claude-opus-4-7")
	if !strings.Contains(lastBlock(m), "switched to anthropic / claude-opus-4-7") {
		t.Fatalf("output = %q, want switch confirmation", lastBlock(m))
	}
	provider, modelID := m.orch.ActiveModelInfo()
	if provider != "anthropic" || modelID != "claude-opus-4-7" {
		t.Fatalf("ActiveModelInfo() = (%q, %q), want (anthropic, claude-opus-4-7)", provider, modelID)
	}
}

func TestHandleModelCommandSingleTokenInfersProvider(t *testing.T) {
	m := newTestModel(t)
	m.handleModelCommand("gpt-4o")
	provider, modelID := m.orch.ActiveModelInfo()
	if provider != "openai" || modelID != "gpt-4o" {
		t.Fatalf("ActiveModelInfo() = (%q, %q), want (openai, gpt-4o)", provider, modelID)
	}
}

func TestHandleModelCommandUnknownSingleTokenErrors(t *testing.T) {
	m := newTestModel(t)
	m.handleModelCommand("not-a-real-model")
	if !strings.Contains(lastBlock(m), "error:") {
		t.Fatalf("output = %q, want an error for an unregistered bare model name", lastBlock(m))
	}
}

func TestHandleLoginCommandAPIKeyThenWhoami(t *testing.T) {
	// Uses the real OS keychain (see newTestModelRealHome's doc comment for
	// why $HOME can't be isolated here) with a per-run disposable provider
	// name, cleaned up afterward, so this never touches or collides with
	// any real credential the host machine has stored.
	m := newTestModelRealHome(t)
	provider := fmt.Sprintf("chronos-code-test-provider-%d", time.Now().UnixNano())
	t.Cleanup(func() { auth.Logout(auth.NewStore(), provider) })

	cmd := m.handleLoginCommand(provider + " sk-ant-test-123")
	if cmd != nil {
		t.Fatal("handleLoginCommand: want nil tea.Cmd for the API-key path (synchronous)")
	}
	if !strings.Contains(lastBlock(m), fmt.Sprintf("stored API key for %q", provider)) {
		t.Fatalf("output = %q, want stored-key confirmation", lastBlock(m))
	}

	m.handleWhoamiCommand(provider)
	who := lastBlock(m)
	if !strings.Contains(who, "chronos-code:api_key") {
		t.Fatalf("whoami output = %q, want it to report the just-stored API key as the active source", who)
	}
}

func TestHandleLoginCommandSubscriptionRejectsAnthropic(t *testing.T) {
	m := newTestModel(t)
	cmd := m.handleLoginCommand("anthropic subscription")
	if cmd != nil {
		t.Fatal("want nil tea.Cmd: anthropic subscription login must be rejected, not attempted")
	}
	if !strings.Contains(lastBlock(m), "only available for openai") {
		t.Fatalf("output = %q, want an explanatory error", lastBlock(m))
	}
}

func TestHandleLoginCommandSubscriptionAcceptsOpenAI(t *testing.T) {
	m := newTestModel(t)
	cmd := m.handleLoginCommand("openai subscription")
	if cmd == nil {
		t.Fatal("handleLoginCommand: want a non-nil tea.Cmd for the openai subscription path (async)")
	}
	if !strings.Contains(lastBlock(m), "starting ChatGPT subscription login") {
		t.Fatalf("output = %q, want a starting message", lastBlock(m))
	}
}

func TestWizardOffersChatGPTSubscriptionOption(t *testing.T) {
	m := newTestModel(t)
	w := newLoginWizard(m)
	found := false
	for _, it := range w.items {
		if it.value == subscriptionLoginValue {
			found = true
			if !strings.Contains(it.label, "ChatGPT") {
				t.Errorf("item label = %q, want it to mention ChatGPT", it.label)
			}
		}
	}
	if !found {
		t.Fatalf("items = %+v, want a ChatGPT subscription entry", w.items)
	}
}

func TestWizardSelectingSubscriptionClosesWizardAndReturnsAsyncCmd(t *testing.T) {
	m := newTestModel(t)
	m.wizard = newLoginWizard(m)
	_, cmd := selectWizardItem(t, m, subscriptionLoginValue)
	if m.wizard != nil {
		t.Fatal("wizard should close immediately for the subscription branch")
	}
	if cmd == nil {
		t.Fatal("want a non-nil tea.Cmd (the async OAuth login)")
	}
	if !strings.Contains(lastBlock(m), "starting ChatGPT subscription login") {
		t.Fatalf("output = %q, want a starting message", lastBlock(m))
	}
}

func TestHandleLoginCommandRequiresArgs(t *testing.T) {
	m := newTestModel(t)
	cmd := m.handleLoginCommand("anthropic")
	if cmd != nil {
		t.Fatal("want nil tea.Cmd for a malformed /login")
	}
	if !strings.Contains(lastBlock(m), "usage:") {
		t.Fatalf("output = %q, want a usage error", lastBlock(m))
	}
}

func TestHandleLoginCommandOAuthReturnsAsyncCmd(t *testing.T) {
	m := newTestModel(t)
	cmd := m.handleLoginCommand("anthropic oauth client-1 https://idp.example.com/authorize https://idp.example.com/token")
	if cmd == nil {
		t.Fatal("handleLoginCommand: want a non-nil tea.Cmd for the oauth path")
	}
	if !strings.Contains(lastBlock(m), "starting OAuth login") {
		t.Fatalf("output = %q, want a starting-OAuth message", lastBlock(m))
	}
}

func TestHandleWhoamiDefaultsToActiveProvider(t *testing.T) {
	m := newTestModel(t)
	m.handleWhoamiCommand("")
	out := lastBlock(m)
	if !strings.HasPrefix(out, "anthropic:") {
		t.Fatalf("output = %q, want it to default to the active agent's provider (anthropic)", out)
	}
}

func TestHandleContextCommandShowsModelAndWindow(t *testing.T) {
	m := newTestModel(t)
	m.handleContextCommand()
	out := lastBlock(m)
	if !strings.Contains(out, "model: anthropic / claude-sonnet-4-6") {
		t.Fatalf("output = %q, want model line", out)
	}
	if !strings.Contains(out, "context window: 200.0k tokens") {
		t.Fatalf("output = %q, want context window line", out)
	}
}

func TestContextUsageSegmentEmptyBeforeAnyTurn(t *testing.T) {
	m := newTestModel(t)
	if seg := m.contextUsageSegment(); seg != "" {
		t.Fatalf("contextUsageSegment() = %q, want empty before any turn completes", seg)
	}
}

func TestContextUsageSegmentAfterUsage(t *testing.T) {
	m := newTestModel(t)
	m.lastKnownUsage.PromptTokens = 12345
	m.lastKnownUsage.CompletionTokens = 200
	seg := m.contextUsageSegment()
	if !strings.Contains(seg, "12.5k") || !strings.Contains(seg, "200.0k") || !strings.Contains(seg, "%") {
		t.Fatalf("contextUsageSegment() = %q, want used/window (pct%%) for a known model", seg)
	}
}

func TestRenderHeaderBarIncludesActiveModel(t *testing.T) {
	m := newTestModel(t)
	m.width = 80
	header := m.renderHeaderBar()
	if !strings.Contains(header, "anthropic/claude-sonnet-4-6") {
		t.Fatalf("header = %q, want it to include the active provider/model", header)
	}
}

// wizardItemIndex returns the index of the item with the given value, or
// fails the test if it isn't present.
func wizardItemIndex(t *testing.T, w *loginWizard, value string) int {
	t.Helper()
	for i, it := range w.items {
		if it.value == value {
			return i
		}
	}
	t.Fatalf("items = %+v, want one with value %q", w.items, value)
	return -1
}

// selectWizardItem navigates the wizard's current step to the item with
// the given value and presses enter, so tests don't depend on fixed
// item-order indices.
func selectWizardItem(t *testing.T, m *appModel, value string) (tea.Model, tea.Cmd) {
	t.Helper()
	m.wizard.idx = wizardItemIndex(t, m.wizard, value)
	return m.handleWizardKey(tea.KeyMsg{Type: tea.KeyEnter})
}

func TestNewLoginWizardOffersAPIKeyAndOAuthWithNoDetectedLogins(t *testing.T) {
	m := newTestModel(t) // isolated $HOME: no ~/.claude or ~/.codex to detect.
	w := newLoginWizard(m)
	if len(w.items) != 3 {
		t.Fatalf("items = %+v, want exactly [subscription, apikey, oauth] with nothing detected", w.items)
	}
	wizardItemIndex(t, w, subscriptionLoginValue)
	wizardItemIndex(t, w, "apikey")
	wizardItemIndex(t, w, "oauth")
}

func TestNewLoginWizardSurfacesDetectedClaudeCodeLogin(t *testing.T) {
	m := newTestModel(t) // isolated $HOME from buildTestModel's t.Setenv("HOME", ...) chain.
	home := os.Getenv("HOME")
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-test","expiresAt":9999999999999}}`
	if err := os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	w := newLoginWizard(m)
	if len(w.items) != 4 {
		t.Fatalf("items = %+v, want [existing:anthropic, subscription, apikey, oauth]", w.items)
	}
	if w.items[0].value != "existing:anthropic" {
		t.Fatalf("items[0] = %+v, want the detected Claude Code login first", w.items[0])
	}

	// Selecting it should report whoami status and close the wizard, not
	// advance to a provider/text-input step (there's nothing to configure —
	// it's already active).
	m.wizard = w
	m.handleWizardKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.wizard != nil {
		t.Fatal("wizard should close immediately for the existing-login branch")
	}
	if !strings.Contains(lastBlock(m), "reuse:external_cli") {
		t.Fatalf("output = %q, want it to report the reused external credential as the active source", lastBlock(m))
	}
}

func TestWizardNavigationIsBounded(t *testing.T) {
	m := newTestModel(t)
	m.wizard = newLoginWizard(m)

	m.handleWizardKey(tea.KeyMsg{Type: tea.KeyUp}) // already at 0; must not go negative
	if m.wizard.idx != 0 {
		t.Fatalf("idx = %d, want 0 (bounded at top)", m.wizard.idx)
	}

	for range m.wizard.items {
		m.handleWizardKey(tea.KeyMsg{Type: tea.KeyDown})
	}
	if m.wizard.idx != len(m.wizard.items)-1 {
		t.Fatalf("idx = %d, want %d (bounded at bottom)", m.wizard.idx, len(m.wizard.items)-1)
	}
}

func TestWizardEscCancelsAtAnyStep(t *testing.T) {
	m := newTestModel(t)
	m.wizard = newLoginWizard(m)
	m.handleWizardKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.wizard != nil {
		t.Fatal("wizard survived Esc at stepAuthMethod")
	}

	m.wizard = newLoginWizard(m)
	selectWizardItem(t, m, "apikey")
	if m.wizard.step != stepProvider {
		t.Fatalf("step = %v, want stepProvider", m.wizard.step)
	}
	m.handleWizardKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.wizard != nil {
		t.Fatal("wizard survived Esc at stepProvider")
	}
}

func TestWizardAPIKeyPathReachesTextInputWithPasswordEcho(t *testing.T) {
	m := newTestModel(t)
	m.wizard = newLoginWizard(m)
	selectWizardItem(t, m, "apikey")
	if m.wizard.method != "apikey" || m.wizard.step != stepProvider {
		t.Fatalf("wizard = %+v, want method=apikey, step=stepProvider", m.wizard)
	}
	// Select the first provider in the list.
	provider := m.wizard.items[0].value
	m.handleWizardKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.wizard.step != stepTextInput || m.wizard.provider != provider {
		t.Fatalf("wizard = %+v, want stepTextInput for provider %q", m.wizard, provider)
	}
	if m.wizard.input.EchoMode != textinput.EchoPassword {
		t.Fatal("API key input should mask its value (EchoPassword)")
	}
}

func TestWizardOAuthPathReachesTextInputWithoutPasswordEcho(t *testing.T) {
	m := newTestModel(t)
	m.wizard = newLoginWizard(m)
	selectWizardItem(t, m, "oauth")
	if m.wizard.method != "oauth" {
		t.Fatalf("method = %q, want oauth", m.wizard.method)
	}
	m.handleWizardKey(tea.KeyMsg{Type: tea.KeyEnter}) // pick first provider
	if m.wizard.input.EchoMode != textinput.EchoNormal {
		t.Fatal("OAuth config input should be plain text, not masked")
	}
}

func TestWizardSubmitAPIKeyEndToEnd(t *testing.T) {
	// Real keychain again (see TestHandleLoginCommandAPIKeyThenWhoami); uses
	// a disposable provider name, cleaned up afterward.
	m := newTestModelRealHome(t)
	provider := fmt.Sprintf("chronos-code-test-wizard-%d", time.Now().UnixNano())
	t.Cleanup(func() { auth.Logout(auth.NewStore(), provider) })

	m.wizard = &loginWizard{step: stepTextInput, method: "apikey", provider: provider, input: newTextPrompt("key", true)}
	m.wizard.input.SetValue("sk-test-wizard-key")

	if _, cmd := m.handleWizardKey(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatal("want nil tea.Cmd for the synchronous API-key submit path")
	}
	if m.wizard != nil {
		t.Fatal("wizard should close after submit")
	}
	if !strings.Contains(lastBlock(m), fmt.Sprintf("stored API key for %q", provider)) {
		t.Fatalf("output = %q, want stored-key confirmation", lastBlock(m))
	}
}

func TestWizardSubmitOAuthReturnsAsyncCmd(t *testing.T) {
	m := newTestModel(t)
	m.wizard = &loginWizard{step: stepTextInput, method: "oauth", provider: "anthropic", input: newTextPrompt("cfg", false)}
	m.wizard.input.SetValue("client-1 https://idp.example.com/authorize https://idp.example.com/token")

	_, cmd := m.handleWizardKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("want a non-nil tea.Cmd for the async OAuth submit path")
	}
	if m.wizard != nil {
		t.Fatal("wizard should close after submit")
	}
}

func TestFormatTokenCount(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "unknown"},
		{-1, "unknown"},
		{500, "500"},
		{12345, "12.3k"},
		{200_000, "200.0k"},
		{1_000_000, "1.0M"},
	}
	for _, c := range cases {
		if got := formatTokenCount(c.in); got != c.want {
			t.Errorf("formatTokenCount(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
