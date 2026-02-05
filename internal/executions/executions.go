package executions

import (
	"errors"
	"sync"
	"time"
)

// Status describes execution status.
type Status string

const (
	// StatusSuccess means execution finished successfully.
	StatusSuccess Status = "success"
	// StatusError means execution failed.
	StatusError Status = "error"
	// StatusPending means execution is queued for async completion.
	StatusPending Status = "pending"
)

// Callback defines async callback settings.
type Callback struct {
	// URL is the webhook callback URL.
	URL string `json:"url"`
}

// Tool describes tool metadata from yaml-mcp-server.
type Tool struct {
	Name         string         `json:"name"`
	Title        string         `json:"title,omitempty"`
	Description  string         `json:"description,omitempty"`
	InputSchema  map[string]any `json:"input_schema,omitempty"`
	OutputSchema map[string]any `json:"output_schema,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	Tags         []string       `json:"tags,omitempty"`
}

// Request holds data required for execution.
type Request struct {
	CorrelationID string
	Tool          Tool
	Arguments     map[string]any
	Spec          map[string]any
	Question      string
	Context       string
	Options       []string
	AllowCustom   bool
	CustomLabel   string
	Lang          string
	Markup        string
	Callback      Callback
}

// Result represents the execution result.
type Result struct {
	Status Status
	Output any
	Note   string
}

// Execution stores state for a single execution request.
type Execution struct {
	Request      Request
	CreatedAt    time.Time
	MessageID    int
	MessageText  string
	AwaitingText bool
}

// Registry stores active execution requests.
type Registry struct {
	mu                sync.Mutex
	executions        map[string]*Execution
	promptMessageID   int
	promptCorrelation string
}

// ErrAlreadyExists is returned when correlation id already exists.
var ErrAlreadyExists = errors.New("execution already exists")

// NewRegistry creates a new execution registry.
func NewRegistry() *Registry {
	return &Registry{executions: make(map[string]*Execution)}
}

// Add registers a new execution request.
func (r *Registry) Add(req Request) (*Execution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.executions[req.CorrelationID]; exists {
		return nil, ErrAlreadyExists
	}
	exec := &Execution{Request: req, CreatedAt: time.Now()}
	r.executions[req.CorrelationID] = exec
	return exec, nil
}

// Get returns execution by correlation id.
func (r *Registry) Get(correlationID string) *Execution {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.executions[correlationID]
}

// SetMessage stores Telegram message metadata for execution.
func (r *Registry) SetMessage(correlationID string, messageID int, messageText string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if exec, ok := r.executions[correlationID]; ok {
		exec.MessageID = messageID
		exec.MessageText = messageText
	}
}

// StartCustomInput marks execution as waiting for custom text and returns previous prompt to delete.
func (r *Registry) StartCustomInput(correlationID string) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	exec, ok := r.executions[correlationID]
	if !ok {
		return 0, false
	}
	var previousPrompt int
	if r.promptCorrelation != "" && r.promptCorrelation != correlationID {
		if prevExec, exists := r.executions[r.promptCorrelation]; exists {
			prevExec.AwaitingText = false
		}
		previousPrompt = r.promptMessageID
	}
	exec.AwaitingText = true
	r.promptCorrelation = correlationID
	r.promptMessageID = 0
	return previousPrompt, true
}

// SetPromptMessage stores active custom-input prompt message id.
func (r *Registry) SetPromptMessage(correlationID string, messageID int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.promptCorrelation == correlationID {
		r.promptMessageID = messageID
	}
}

// ClearPrompt removes active custom-input prompt if correlation id matches.
func (r *Registry) ClearPrompt(correlationID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.promptCorrelation != correlationID {
		return 0
	}
	if exec, ok := r.executions[correlationID]; ok {
		exec.AwaitingText = false
	}
	removed := r.promptMessageID
	r.promptMessageID = 0
	r.promptCorrelation = ""
	return removed
}

// CurrentPrompt returns execution awaiting custom input and prompt message id.
func (r *Registry) CurrentPrompt() (*Execution, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.promptCorrelation == "" {
		return nil, 0
	}
	exec := r.executions[r.promptCorrelation]
	if exec == nil || !exec.AwaitingText {
		return nil, 0
	}
	return exec, r.promptMessageID
}

// Resolve removes execution and clears prompt if needed.
func (r *Registry) Resolve(correlationID string) (*Execution, int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	exec, ok := r.executions[correlationID]
	if !ok {
		return nil, 0, false
	}
	delete(r.executions, correlationID)
	promptID := 0
	if r.promptCorrelation == correlationID {
		promptID = r.promptMessageID
		r.promptMessageID = 0
		r.promptCorrelation = ""
	}
	return exec, promptID, true
}
