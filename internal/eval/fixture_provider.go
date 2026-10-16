package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"

	"github.com/spawn08/chronos-code/internal/config"
	"gopkg.in/yaml.v3"
)

// FixtureProvider serves a pinned OpenAI-compatible script over loopback only.
// The orchestrator, SDK tool loop and real tools still execute normally.
type FixtureProvider struct {
	server  *httptest.Server
	mu      sync.Mutex
	next    int
	script  fixtureScript
	failure string
}

type fixtureScript struct {
	Version   string `yaml:"version"`
	Model     string `yaml:"model"`
	Responses []struct {
		Content   string         `yaml:"content"`
		Tool      string         `yaml:"tool"`
		Arguments map[string]any `yaml:"arguments"`
	} `yaml:"responses"`
}

func NewFixtureProvider(path string) (*FixtureProvider, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eval: read provider fixture: %w", err)
	}
	var script fixtureScript
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&script); err != nil {
		return nil, fmt.Errorf("eval: decode provider fixture: %w", err)
	}
	if script.Version != "chronos.eval.provider.v1" || script.Model == "" || len(script.Responses) == 0 {
		return nil, fmt.Errorf("eval: invalid provider fixture")
	}
	provider := &FixtureProvider{script: script}
	provider.server = httptest.NewServer(http.HandlerFunc(provider.serve))
	return provider, nil
}

func (p *FixtureProvider) Close() { p.server.Close() }
func (p *FixtureProvider) Model() ModelSettings {
	return ModelSettings{Provider: "openai", Name: p.script.Model}
}

func (p *FixtureProvider) Configure(cfg *config.Config) error {
	if err := cfg.OverridePrimaryModel("openai", p.script.Model, "fixture", "fixture"); err != nil {
		return err
	}
	for i := range cfg.Agents {
		cfg.Agents[i].Model.APIKey = "fixture-not-a-credential"
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]config.ProviderOverride{}
	}
	cfg.Providers["openai"] = config.ProviderOverride{BaseURL: p.server.URL + "/v1"}
	return nil
}

func (p *FixtureProvider) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		p.failure = fmt.Sprintf("unexpected endpoint: %s %s", r.Method, r.URL.Path)
		http.Error(w, p.failure, http.StatusNotFound)
		return
	}
	var request struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		Tools  []struct {
			Name     string `json:"name"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if request.Model != p.script.Model || request.Stream || p.next >= len(p.script.Responses) {
		p.failure = fmt.Sprintf("unexpected model=%s stream=%v request=%d", request.Model, request.Stream, p.next)
		http.Error(w, p.failure, http.StatusBadRequest)
		return
	}
	response := p.script.Responses[p.next]
	message := map[string]any{"role": "assistant", "content": response.Content}
	finish := "stop"
	if response.Tool != "" {
		found := false
		var names []string
		for _, definition := range request.Tools {
			names = append(names, definition.Name+"/"+definition.Function.Name)
			if definition.Function.Name == response.Tool || definition.Name == response.Tool {
				found = true
			}
		}
		if !found {
			p.failure = fmt.Sprintf("scripted tool absent from runtime schema: %s (available %v)", response.Tool, names)
			http.Error(w, p.failure, http.StatusBadRequest)
			return
		}
		arguments, err := json.Marshal(response.Arguments)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		message["tool_calls"] = []any{map[string]any{"id": fmt.Sprintf("fixture-tool-%d", p.next+1), "type": "function", "function": map[string]any{"name": response.Tool, "arguments": string(arguments)}}}
		finish = "tool_calls"
	}
	p.next++
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprintf("fixture-model-%d", p.next), "object": "chat.completion", "model": p.script.Model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"usage":   map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}})
}

func (p *FixtureProvider) Failure() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failure
}

func (p *FixtureProvider) Complete() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.next == len(p.script.Responses)
}
