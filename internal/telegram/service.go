package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/codex-k8s/telegram-executor/internal/config"
	"github.com/codex-k8s/telegram-executor/internal/executions"
	"github.com/codex-k8s/telegram-executor/internal/i18n"
	"github.com/codex-k8s/telegram-executor/internal/telegram/handlers"
	"github.com/codex-k8s/telegram-executor/internal/telegram/updates"
	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

const timeoutResult = "execution timeout"

// Service manages Telegram bot lifecycle and execution requests.
type Service struct {
	bot      *telego.Bot
	source   updates.Source
	handler  *handlers.Handler
	registry *executions.Registry
	log      *slog.Logger
	messages map[string]i18n.Messages
	lang     string
	chatID   int64
}

// New creates a new Telegram service.
func New(cfg config.Config, bundle i18n.Bundle, registry *executions.Registry, log *slog.Logger) (*Service, error) {
	bot, err := telego.NewBot(cfg.Token, telego.WithLogger(telegoLogger{log: log}))
	if err != nil {
		return nil, err
	}

	var source updates.Source
	if cfg.WebhookEnabled() {
		source = updates.NewWebhook(bot, cfg.WebhookURL, cfg.WebhookSecret, log)
	} else {
		source = updates.NewLongPolling(bot, log)
	}

	var transcriber handlers.Transcriber
	if cfg.OpenAIAPIKey != "" {
		transcriber = handlers.NewOpenAITranscriber(cfg.OpenAIAPIKey, cfg.STTModel, cfg.STTTimeout, log)
	}

	sttLang := cfg.Lang
	if sttLang == "" {
		sttLang = "en"
	}

	messages := map[string]i18n.Messages{bundle.Lang: bundle.Messages}
	if bundle.Lang != "en" {
		if extra, err := i18n.Load("en"); err == nil {
			messages[extra.Lang] = extra.Messages
		}
	}
	if bundle.Lang != "ru" {
		if extra, err := i18n.Load("ru"); err == nil {
			messages[extra.Lang] = extra.Messages
		}
	}

	handler := handlers.NewHandler(bot, registry, messages, cfg.Lang, cfg.ChatID, sttLang, transcriber, log)

	return &Service{
		bot:      bot,
		source:   source,
		handler:  handler,
		registry: registry,
		log:      log,
		messages: messages,
		lang:     cfg.Lang,
		chatID:   cfg.ChatID,
	}, nil
}

// Start begins receiving Telegram updates.
func (s *Service) Start(ctx context.Context) error {
	if err := s.source.Start(ctx); err != nil {
		return err
	}
	go s.handler.Run(ctx, s.source.Updates())
	return nil
}

// Stop shuts down Telegram update processing.
func (s *Service) Stop(ctx context.Context) error {
	return s.source.Stop(ctx)
}

// WebhookHandler returns the webhook HTTP handler if enabled.
func (s *Service) WebhookHandler() http.Handler {
	return s.source.Handler()
}

// SubmitExecution sends execution request to Telegram and returns immediately.
func (s *Service) SubmitExecution(ctx context.Context, req executions.Request, timeout time.Duration, timeoutMessage string) (executions.Result, error) {
	if timeout <= 0 {
		timeout = time.Hour
	}
	_, err := s.registry.Add(req)
	if err != nil {
		return executions.Result{Status: executions.StatusError, Output: "execution already exists"}, nil
	}

	messageText := s.renderMessage(req)
	keyboard := s.optionsKeyboard(req)
	parseMode := parseMode(req.Markup)

	msg, err := s.bot.SendMessage(ctx, &telego.SendMessageParams{
		ChatID:      tu.ID(s.chatID),
		Text:        messageText,
		ParseMode:   parseMode,
		ReplyMarkup: keyboard,
	})
	if err != nil {
		s.log.Error("Failed to send telegram message", "error", err)
		return executions.Result{Status: executions.StatusError, Output: "failed to send telegram message"}, err
	}

	s.registry.SetMessage(req.CorrelationID, msg.MessageID, messageText)
	s.scheduleTimeout(req.CorrelationID, timeout, timeoutMessage)
	return executions.Result{Status: executions.StatusPending, Output: "queued"}, nil
}

func (s *Service) renderMessage(req executions.Request) string {
	payload, err := json.MarshalIndent(req.Arguments, "", "  ")
	if err != nil {
		payload = []byte("{}")
	}
	msg := s.messagesFor(req.Lang)
	switch strings.ToLower(strings.TrimSpace(req.Markup)) {
	case "html":
		return renderHTML(msg, req, payload)
	default:
		return renderMarkdown(msg, req, payload)
	}
}

func (s *Service) optionsKeyboard(req executions.Request) *telego.InlineKeyboardMarkup {
	msg := s.messagesFor(req.Lang)
	rows := make([][]telego.InlineKeyboardButton, 0, len(req.Options)+1)
	for idx, option := range req.Options {
		payload := fmt.Sprintf("%s|%d", req.CorrelationID, idx)
		label := fmt.Sprintf("%d. %s", idx+1, shortenButtonLabel(option, 42))
		rows = append(rows, tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(label).WithCallbackData(handlers.CallbackData(handlers.ActionOption, payload)),
		))
	}
	if req.AllowCustom {
		customLabel := strings.TrimSpace(req.CustomLabel)
		if customLabel == "" {
			customLabel = msg.CustomOptionButton
		}
		if customLabel == "" {
			customLabel = "Custom option"
		}
		rows = append(rows, tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(customLabel).WithCallbackData(handlers.CallbackData(handlers.ActionCustom, req.CorrelationID)),
		))
	}
	return tu.InlineKeyboard(rows...)
}

func shortenButtonLabel(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	if maxRunes <= 0 {
		return value
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	if maxRunes <= 1 {
		return string(runes[:maxRunes])
	}
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-3]) + "..."
}

func (s *Service) scheduleTimeout(correlationID string, timeout time.Duration, timeoutMessage string) {
	go func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		<-timer.C
		exec, promptID, ok := s.registry.Resolve(correlationID)
		if !ok {
			return
		}
		if promptID > 0 {
			_ = s.handler.DeleteMessage(context.Background(), promptID)
		}
		s.handler.FinalizeExecution(context.Background(), exec, executions.Result{
			Status: executions.StatusError,
			Output: timeoutResult,
		}, timeoutMessage)
	}()
}

func (s *Service) messagesFor(lang string) i18n.Messages {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if lang == "" {
		lang = s.lang
	}
	if msg, ok := s.messages[lang]; ok {
		return msg
	}
	if msg, ok := s.messages["en"]; ok {
		return msg
	}
	return i18n.Messages{}
}

func parseMode(markup string) string {
	switch strings.ToLower(strings.TrimSpace(markup)) {
	case "html":
		return telego.ModeHTML
	default:
		return telego.ModeMarkdownV2
	}
}

func renderMarkdown(msg i18n.Messages, req executions.Request, payload []byte) string {
	builder := &strings.Builder{}
	builder.WriteString("*")
	builder.WriteString(escapeMarkdownV2(msg.ExecutionTitle))
	builder.WriteString("*\n\n")

	contextTitle := msg.SectionContext
	if strings.TrimSpace(contextTitle) == "" {
		contextTitle = "Context"
	}
	actionTitle := msg.SectionAction
	if strings.TrimSpace(actionTitle) == "" {
		actionTitle = "Action"
	}
	paramsTitle := msg.SectionParams
	if strings.TrimSpace(paramsTitle) == "" {
		paramsTitle = msg.ExecutionParams
	}
	if strings.TrimSpace(paramsTitle) == "" {
		paramsTitle = "Parameters"
	}

	builder.WriteString("*")
	builder.WriteString(escapeMarkdownV2(contextTitle))
	builder.WriteString("*\n")

	questionLabel := msg.QuestionLabel
	if strings.TrimSpace(questionLabel) == "" {
		questionLabel = "Question"
	}
	builder.WriteString("*")
	builder.WriteString(escapeMarkdownV2(questionLabel))
	builder.WriteString(":* ")
	builder.WriteString(escapeMarkdownV2(req.Question))
	builder.WriteString("\n")

	if strings.TrimSpace(req.Context) != "" {
		contextLabel := msg.ContextLabel
		if strings.TrimSpace(contextLabel) == "" {
			contextLabel = "Context"
		}
		builder.WriteString("*")
		builder.WriteString(escapeMarkdownV2(contextLabel))
		builder.WriteString(":* ")
		builder.WriteString(escapeMarkdownV2(req.Context))
		builder.WriteString("\n")
	}

	optionsLabel := msg.OptionsLabel
	if strings.TrimSpace(optionsLabel) == "" {
		optionsLabel = "Options"
	}
	builder.WriteString("*")
	builder.WriteString(escapeMarkdownV2(optionsLabel))
	builder.WriteString(":*\n")
	for idx, option := range req.Options {
		builder.WriteString(fmt.Sprintf("%d\\) %s\n", idx+1, escapeMarkdownV2(option)))
	}
	builder.WriteString("\n")

	builder.WriteString("*")
	builder.WriteString(escapeMarkdownV2(actionTitle))
	builder.WriteString("*\n")
	builder.WriteString("*")
	builder.WriteString(escapeMarkdownV2(msg.ExecutionTool))
	builder.WriteString(":* `")
	builder.WriteString(escapeMarkdownV2Code(req.Tool.Name))
	builder.WriteString("`\n")
	builder.WriteString("*")
	builder.WriteString(escapeMarkdownV2(msg.ExecutionCorrelation))
	builder.WriteString(":* `")
	builder.WriteString(escapeMarkdownV2Code(req.CorrelationID))
	builder.WriteString("`\n\n")

	builder.WriteString("*")
	builder.WriteString(escapeMarkdownV2(paramsTitle))
	builder.WriteString("*\n\n```json\n")
	builder.WriteString(escapeMarkdownV2CodeBlock(string(payload)))
	builder.WriteString("\n```")
	return builder.String()
}

func renderHTML(msg i18n.Messages, req executions.Request, payload []byte) string {
	builder := &strings.Builder{}
	builder.WriteString("<b>")
	builder.WriteString(htmlEscape(msg.ExecutionTitle))
	builder.WriteString("</b><br><br>")

	contextTitle := msg.SectionContext
	if strings.TrimSpace(contextTitle) == "" {
		contextTitle = "Context"
	}
	actionTitle := msg.SectionAction
	if strings.TrimSpace(actionTitle) == "" {
		actionTitle = "Action"
	}
	paramsTitle := msg.SectionParams
	if strings.TrimSpace(paramsTitle) == "" {
		paramsTitle = msg.ExecutionParams
	}
	if strings.TrimSpace(paramsTitle) == "" {
		paramsTitle = "Parameters"
	}

	builder.WriteString("<b>")
	builder.WriteString(htmlEscape(contextTitle))
	builder.WriteString("</b><br>")

	questionLabel := msg.QuestionLabel
	if strings.TrimSpace(questionLabel) == "" {
		questionLabel = "Question"
	}
	builder.WriteString("<b>")
	builder.WriteString(htmlEscape(questionLabel))
	builder.WriteString(":</b> ")
	builder.WriteString(htmlEscape(req.Question))
	builder.WriteString("<br>")

	if strings.TrimSpace(req.Context) != "" {
		contextLabel := msg.ContextLabel
		if strings.TrimSpace(contextLabel) == "" {
			contextLabel = "Context"
		}
		builder.WriteString("<b>")
		builder.WriteString(htmlEscape(contextLabel))
		builder.WriteString(":</b> ")
		builder.WriteString(htmlEscape(req.Context))
		builder.WriteString("<br>")
	}

	optionsLabel := msg.OptionsLabel
	if strings.TrimSpace(optionsLabel) == "" {
		optionsLabel = "Options"
	}
	builder.WriteString("<b>")
	builder.WriteString(htmlEscape(optionsLabel))
	builder.WriteString(":</b><br>")
	for idx, option := range req.Options {
		builder.WriteString(fmt.Sprintf("%d) %s<br>", idx+1, htmlEscape(option)))
	}
	builder.WriteString("<br>")

	builder.WriteString("<b>")
	builder.WriteString(htmlEscape(actionTitle))
	builder.WriteString("</b><br>")
	builder.WriteString("<b>")
	builder.WriteString(htmlEscape(msg.ExecutionTool))
	builder.WriteString(":</b> <code>")
	builder.WriteString(htmlEscape(req.Tool.Name))
	builder.WriteString("</code><br>")
	builder.WriteString("<b>")
	builder.WriteString(htmlEscape(msg.ExecutionCorrelation))
	builder.WriteString(":</b> <code>")
	builder.WriteString(htmlEscape(req.CorrelationID))
	builder.WriteString("</code><br><br>")

	builder.WriteString("<b>")
	builder.WriteString(htmlEscape(paramsTitle))
	builder.WriteString("</b><br><pre><code>")
	builder.WriteString(htmlEscape(string(payload)))
	builder.WriteString("</code></pre>")
	return builder.String()
}

func htmlEscape(value string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return replacer.Replace(value)
}

func escapeMarkdownV2(value string) string {
	if value == "" {
		return value
	}
	var builder strings.Builder
	builder.Grow(len(value) * 2)
	for _, r := range value {
		switch r {
		case '_', '*', '[', ']', '(', ')', '~', '`', '>', '#', '+', '-', '=', '|', '{', '}', '.', '!', '\\':
			builder.WriteByte('\\')
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

func escapeMarkdownV2Code(value string) string {
	if value == "" {
		return value
	}
	var builder strings.Builder
	builder.Grow(len(value) * 2)
	for _, r := range value {
		switch r {
		case '\\', '`':
			builder.WriteByte('\\')
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

func escapeMarkdownV2CodeBlock(value string) string {
	if value == "" {
		return value
	}
	var builder strings.Builder
	builder.Grow(len(value) * 2)
	for _, r := range value {
		switch r {
		case '\\', '`':
			builder.WriteByte('\\')
		}
		builder.WriteRune(r)
	}
	return builder.String()
}
