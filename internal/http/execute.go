package http

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/codex-k8s/telegram-executor/internal/config"
	"github.com/codex-k8s/telegram-executor/internal/executions"
	"github.com/codex-k8s/telegram-executor/internal/telegram"
)

// ExecuteHandler handles execution requests from yaml-mcp-server.
type ExecuteHandler struct {
	svc *telegram.Service
	cfg config.Config
	log *slog.Logger
}

// NewExecuteHandler creates a new execution handler.
func NewExecuteHandler(svc *telegram.Service, cfg config.Config, log *slog.Logger) *ExecuteHandler {
	return &ExecuteHandler{svc: svc, cfg: cfg, log: log}
}

// ExecuteRequest defines input payload for /execute.
type ExecuteRequest struct {
	CorrelationID string               `json:"correlation_id"`
	Tool          executions.Tool      `json:"tool"`
	Arguments     map[string]any       `json:"arguments"`
	Spec          map[string]any       `json:"spec,omitempty"`
	Lang          string               `json:"lang,omitempty"`
	Markup        string               `json:"markup,omitempty"`
	Callback      *executions.Callback `json:"callback,omitempty"`
	TimeoutSec    int                  `json:"timeout_sec,omitempty"`
}

// ExecuteResponse defines output payload for /execute.
type ExecuteResponse struct {
	Status        string `json:"status"`
	Result        any    `json:"result,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// ServeHTTP handles /execute requests.
func (h *ExecuteHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req ExecuteRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&req); err != nil {
		h.respond(w, http.StatusBadRequest, executions.StatusError, "invalid json payload")
		return
	}
	if strings.TrimSpace(req.CorrelationID) == "" {
		h.respond(w, http.StatusBadRequest, executions.StatusError, "correlation_id is required")
		return
	}
	if strings.TrimSpace(req.Tool.Name) == "" {
		h.respond(w, http.StatusBadRequest, executions.StatusError, "tool.name is required")
		return
	}
	if req.Arguments == nil {
		req.Arguments = map[string]any{}
	}
	if strings.TrimSpace(req.Markup) == "" {
		req.Markup = "markdown"
	}
	switch strings.ToLower(strings.TrimSpace(req.Markup)) {
	case "markdown", "html":
	default:
		h.respond(w, http.StatusBadRequest, executions.StatusError, "markup must be markdown or html")
		return
	}
	req.Lang = normalizeLang(req.Lang, h.cfg.Lang)
	if req.Callback == nil || strings.TrimSpace(req.Callback.URL) == "" {
		h.respond(w, http.StatusBadRequest, executions.StatusError, "callback.url is required for async execution")
		return
	}

	question, contextValue, options, allowCustom, err := parseFeedbackArgs(req.Arguments, req.Spec)
	if err != nil {
		h.respond(w, http.StatusBadRequest, executions.StatusError, err.Error())
		return
	}

	timeout := h.cfg.ExecutionTimeout
	if req.TimeoutSec > 0 {
		timeout = time.Duration(req.TimeoutSec) * time.Second
	}

	ctx := r.Context()
	res, err := h.svc.SubmitExecution(ctx, executions.Request{
		CorrelationID: req.CorrelationID,
		Tool:          req.Tool,
		Arguments:     req.Arguments,
		Spec:          req.Spec,
		Question:      question,
		Context:       contextValue,
		Options:       options,
		AllowCustom:   allowCustom,
		Lang:          req.Lang,
		Markup:        req.Markup,
		Callback:      *req.Callback,
	}, timeout, h.cfg.TimeoutMessage)
	if err != nil {
		h.log.Error("Execution request failed", "error", err)
		if res.Status == "" {
			h.respond(w, http.StatusInternalServerError, executions.StatusError, "execution failed")
			return
		}
	}

	h.respond(w, http.StatusAccepted, res.Status, res.Output, req.CorrelationID)
}

func (h *ExecuteHandler) respond(w http.ResponseWriter, statusCode int, status executions.Status, result any, correlationID ...string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	resp := ExecuteResponse{Status: string(status), Result: result}
	if len(correlationID) > 0 {
		resp.CorrelationID = correlationID[0]
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func parseFeedbackArgs(arguments map[string]any, spec map[string]any) (question, contextValue string, options []string, allowCustom bool, err error) {
	question, ok := extractString(arguments, "question")
	if !ok {
		return "", "", nil, false, fmt.Errorf("question is required")
	}
	if len([]rune(question)) < 10 || len([]rune(question)) > 1000 {
		return "", "", nil, false, fmt.Errorf("question must be 10-1000 characters")
	}

	contextValue, _ = extractString(arguments, "context")
	if len([]rune(contextValue)) > 2000 {
		return "", "", nil, false, fmt.Errorf("context must be <= 2000 characters")
	}

	minOptions, maxOptions := optionLimitsFromSpec(spec)
	options, err = extractOptions(arguments, minOptions, maxOptions)
	if err != nil {
		return "", "", nil, false, err
	}

	allowCustom = true
	if value, ok := extractBool(spec, "allow_custom_option"); ok {
		allowCustom = value
	}
	if value, ok := extractBool(arguments, "allow_custom"); ok {
		allowCustom = value
	}
	return question, contextValue, options, allowCustom, nil
}

func optionLimitsFromSpec(spec map[string]any) (int, int) {
	minOptions := 2
	maxOptions := 5
	if value, ok := extractInt(spec, "options_min"); ok && value > 0 {
		minOptions = value
	}
	if value, ok := extractInt(spec, "options_max"); ok && value >= minOptions {
		maxOptions = value
	}
	return minOptions, maxOptions
}

func extractOptions(arguments map[string]any, minOptions, maxOptions int) ([]string, error) {
	raw, ok := arguments["options"]
	if !ok || raw == nil {
		return nil, fmt.Errorf("options is required")
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("options must be array")
	}
	if len(items) < minOptions || len(items) > maxOptions {
		return nil, fmt.Errorf("options count must be %d-%d", minOptions, maxOptions)
	}
	out := make([]string, 0, len(items))
	for idx, item := range items {
		value, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("options[%d] must be string", idx)
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("options[%d] is empty", idx)
		}
		if len([]rune(value)) > 300 {
			return nil, fmt.Errorf("options[%d] must be <= 300 characters", idx)
		}
		out = append(out, value)
	}
	return out, nil
}

func extractString(data map[string]any, key string) (string, bool) {
	if data == nil {
		return "", false
	}
	raw, ok := data[key]
	if !ok {
		return "", false
	}
	value, ok := raw.(string)
	if !ok {
		return "", false
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	return value, true
}

func extractBool(data map[string]any, key string) (bool, bool) {
	if data == nil {
		return false, false
	}
	raw, ok := data[key]
	if !ok {
		return false, false
	}
	value, ok := raw.(bool)
	if !ok {
		return false, false
	}
	return value, true
}

func extractInt(data map[string]any, key string) (int, bool) {
	if data == nil {
		return 0, false
	}
	raw, ok := data[key]
	if !ok || raw == nil {
		return 0, false
	}
	switch value := raw.(type) {
	case int:
		return value, true
	case int32:
		return int(value), true
	case int64:
		return int(value), true
	case float64:
		return int(value), true
	default:
		return 0, false
	}
}

func normalizeLang(value, fallback string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	switch value {
	case "ru", "en":
		return value
	}
	fallback = strings.TrimSpace(strings.ToLower(fallback))
	switch fallback {
	case "ru", "en":
		return fallback
	default:
		return "en"
	}
}
