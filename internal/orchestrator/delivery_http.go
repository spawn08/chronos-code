package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos/engine/tool"
)

const maxDeliveryHTTPResponse = 1 << 20

// deliveryHTTPDestination implements a single configured downstream contract:
// mutations honor the host-issued Idempotency-Key and GET observation_url/{key}
// returns the original {status_code,headers,body} receipt, or 404 if unknown.
// A 404 never authorizes replay of an already-started mutation.
type deliveryHTTPDestination struct {
	requestURL     string
	observationURL string
	client         *http.Client
}

func newDeliveryHTTPTool(cfg config.DeliveryHTTPConfig) (*tool.Definition, error) {
	request, err := url.Parse(cfg.RequestURL)
	if err != nil {
		return nil, fmt.Errorf("delivery HTTP request URL: %w", err)
	}
	observation, err := url.Parse(cfg.ObservationURL)
	if err != nil {
		return nil, fmt.Errorf("delivery HTTP observation URL: %w", err)
	}
	if request.Host == "" || request.Path == "" || request.Host != observation.Host || request.Scheme != observation.Scheme || observation.Path == "" ||
		request.User != nil || observation.User != nil || request.RawQuery != "" || observation.RawQuery != "" || request.Fragment != "" || observation.Fragment != "" ||
		len(cfg.RequestURL) > 4096 || len(cfg.ObservationURL) > 4096 ||
		(request.Scheme != "https" && !(request.Scheme == "http" && (request.Hostname() == "localhost" || net.ParseIP(request.Hostname()) != nil && net.ParseIP(request.Hostname()).IsLoopback()))) {
		return nil, fmt.Errorf("delivery HTTP effect requires same-origin HTTPS request and observation URLs (HTTP allowed on loopback)")
	}
	destination := &deliveryHTTPDestination{requestURL: cfg.RequestURL, observationURL: strings.TrimSuffix(cfg.ObservationURL, "/"), client: &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	return &tool.Definition{
		Name: "http_observed_mutation", Description: "Mutate the configured HTTP destination with a durable idempotency key and observable receipt.",
		Permission: tool.PermRequireApproval, Effects: []tool.Effect{tool.EffectNetwork, tool.EffectExternalMutation}, Recovery: destination,
		Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"method": map[string]any{"type": "string"}, "body": map[string]any{"type": "string"}, "headers": map[string]any{"type": "object"},
		}, "required": []string{"method"}},
		Handler: destination.call,
	}, nil
}

func (d *deliveryHTTPDestination) Prepare(_ context.Context, args map[string]any, effectKey string) (string, error) {
	if effectKey == "" || !deliveryMutationMethod(args) {
		return "", fmt.Errorf("observed HTTP effect requires a mutation method and host-issued key")
	}
	if _, supplied := args["url"]; supplied {
		return "", fmt.Errorf("observed HTTP effect cannot override its configured destination")
	}
	return d.requestURL, nil
}

func deliveryMutationMethod(args map[string]any) bool {
	method, _ := args["method"].(string)
	switch strings.ToUpper(method) {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

func (d *deliveryHTTPDestination) call(ctx context.Context, args map[string]any) (any, error) {
	key, ok := tool.EffectKeyFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("observed HTTP mutation has no host-issued idempotency key")
	}
	if _, err := d.Prepare(ctx, args, key); err != nil {
		return nil, err
	}
	method := strings.ToUpper(args["method"].(string))
	body, _ := args["body"].(string)
	request, err := http.NewRequestWithContext(ctx, method, d.requestURL, strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("prepare observed HTTP request: %w", err)
	}
	if headers, ok := args["headers"].(map[string]any); ok {
		for name, raw := range headers {
			if value, ok := raw.(string); ok && !strings.EqualFold(name, "Idempotency-Key") {
				request.Header.Set(name, value)
			}
		}
	}
	request.Header.Set("Idempotency-Key", key)
	response, err := d.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("observed HTTP mutation: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, fmt.Errorf("observed HTTP destination redirected the effect")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxDeliveryHTTPResponse+1))
	if err != nil {
		return nil, fmt.Errorf("observed HTTP response exceeds limit: %w", err)
	}
	if len(data) > maxDeliveryHTTPResponse {
		return nil, fmt.Errorf("observed HTTP response exceeds limit")
	}
	headers := make(map[string]string, len(response.Header))
	for name := range response.Header {
		headers[name] = response.Header.Get(name)
	}
	return map[string]any{"status_code": response.StatusCode, "headers": headers, "body": string(data)}, nil
}

func (d *deliveryHTTPDestination) Observe(ctx context.Context, descriptor, effectKey string) (any, bool, error) {
	if descriptor != d.requestURL || effectKey == "" {
		return nil, false, fmt.Errorf("observed HTTP descriptor or effect key does not match configured destination")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, d.observationURL+"/"+url.PathEscape(effectKey), nil)
	if err != nil {
		return nil, false, fmt.Errorf("prepare HTTP effect observation: %w", err)
	}
	response, err := d.client.Do(request)
	if err != nil {
		return nil, false, fmt.Errorf("observe HTTP destination: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("observe HTTP destination: status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxDeliveryHTTPResponse+1))
	if err != nil {
		return nil, false, fmt.Errorf("HTTP effect observation exceeds limit: %w", err)
	}
	if len(data) > maxDeliveryHTTPResponse {
		return nil, false, fmt.Errorf("HTTP effect observation exceeds limit")
	}
	var observed struct {
		StatusCode int               `json:"status_code"`
		Headers    map[string]string `json:"headers"`
		Body       *string           `json:"body"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&observed); err != nil {
		return nil, false, fmt.Errorf("decode HTTP effect observation: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF || observed.StatusCode < 100 || observed.StatusCode > 599 || observed.Headers == nil || observed.Body == nil {
		return nil, false, fmt.Errorf("HTTP effect observation has no complete response receipt")
	}
	return map[string]any{"status_code": observed.StatusCode, "headers": observed.Headers, "body": *observed.Body}, true, nil
}
