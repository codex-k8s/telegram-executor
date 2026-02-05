package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/codex-k8s/telegram-executor/internal/config"
	"github.com/codex-k8s/telegram-executor/internal/executions"
	"github.com/codex-k8s/telegram-executor/internal/i18n"
	"github.com/codex-k8s/telegram-executor/internal/telegram/handlers"
	"github.com/codex-k8s/telegram-executor/internal/telegram/shared"
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
	msg := s.messagesFor(req.Lang)
	switch strings.ToLower(strings.TrimSpace(req.Markup)) {
	case "html":
		return renderHTML(msg, req)
	default:
		return renderMarkdown(msg, req)
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
		customLabel := strings.TrimSpace(msg.CustomOptionButton)
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
	return shared.MessagesFor(s.messages, lang, s.lang)
}

func parseMode(markup string) string {
	switch strings.ToLower(strings.TrimSpace(markup)) {
	case "html":
		return telego.ModeHTML
	default:
		return telego.ModeMarkdownV2
	}
}

func renderMarkdown(msg i18n.Messages, req executions.Request) string {
	return renderExecution(msg, req, markdownExecutionWriter{})
}

func renderHTML(msg i18n.Messages, req executions.Request) string {
	return renderExecution(msg, req, htmlExecutionWriter{})
}

func renderExecution(msg i18n.Messages, req executions.Request, writer executionMessageWriter) string {
	labels := executionLabelsFor(msg)
	builder := &strings.Builder{}
	writer.WriteTitle(builder, msg.ExecutionTitle)

	writer.WriteSectionHeader(builder, labels.ContextTitle)
	writer.WriteLabelValue(builder, labels.QuestionLabel, req.Question, false)

	if strings.TrimSpace(req.Context) != "" {
		writer.WriteLabelValue(builder, labels.ContextLabel, req.Context, false)
	}

	writer.WriteOptions(builder, labels.OptionsLabel, req.Options)

	writer.WriteSectionHeader(builder, labels.ActionTitle)
	writer.WriteCodeValue(builder, msg.ExecutionTool, req.Tool.Name, false)
	writer.WriteCodeValue(builder, msg.ExecutionCorrelation, req.CorrelationID, false)
	return builder.String()
}

type executionMessageWriter interface {
	WriteTitle(builder *strings.Builder, title string)
	WriteSectionHeader(builder *strings.Builder, title string)
	WriteLabelValue(builder *strings.Builder, label, value string, addEmptyLine bool)
	WriteOptions(builder *strings.Builder, label string, options []string)
	WriteCodeValue(builder *strings.Builder, label, value string, addEmptyLine bool)
}

type markdownExecutionWriter struct{}

func (markdownExecutionWriter) WriteTitle(builder *strings.Builder, title string) {
	builder.WriteString("*")
	builder.WriteString(shared.EscapeMarkdownV2(title))
	builder.WriteString("*\n\n")
}

func (markdownExecutionWriter) WriteSectionHeader(builder *strings.Builder, title string) {
	builder.WriteString("*")
	builder.WriteString(shared.EscapeMarkdownV2(title))
	builder.WriteString("*\n")
}

func (markdownExecutionWriter) WriteLabelValue(builder *strings.Builder, label, value string, addEmptyLine bool) {
	builder.WriteString("*")
	builder.WriteString(shared.EscapeMarkdownV2(label))
	builder.WriteString(":* ")
	builder.WriteString(shared.EscapeMarkdownV2(value))
	builder.WriteString("\n")
	appendOptionalLineBreak(builder, "\n", addEmptyLine)
}

func (markdownExecutionWriter) WriteOptions(builder *strings.Builder, label string, options []string) {
	builder.WriteString("*")
	builder.WriteString(shared.EscapeMarkdownV2(label))
	builder.WriteString(":*\n")
	for idx, option := range options {
		builder.WriteString(fmt.Sprintf("%d\\) %s\n", idx+1, shared.EscapeMarkdownV2(option)))
	}
	builder.WriteString("\n")
}

func (markdownExecutionWriter) WriteCodeValue(builder *strings.Builder, label, value string, addEmptyLine bool) {
	builder.WriteString("*")
	builder.WriteString(shared.EscapeMarkdownV2(label))
	builder.WriteString(":* `")
	builder.WriteString(shared.EscapeMarkdownV2Code(value))
	builder.WriteString("`\n")
	appendOptionalLineBreak(builder, "\n", addEmptyLine)
}

type htmlExecutionWriter struct{}

func (htmlExecutionWriter) WriteTitle(builder *strings.Builder, title string) {
	builder.WriteString("<b>")
	builder.WriteString(shared.EscapeHTML(title))
	builder.WriteString("</b><br><br>")
}

func (htmlExecutionWriter) WriteSectionHeader(builder *strings.Builder, title string) {
	builder.WriteString("<b>")
	builder.WriteString(shared.EscapeHTML(title))
	builder.WriteString("</b><br>")
}

func (htmlExecutionWriter) WriteLabelValue(builder *strings.Builder, label, value string, addEmptyLine bool) {
	builder.WriteString("<b>")
	builder.WriteString(shared.EscapeHTML(label))
	builder.WriteString(":</b> ")
	builder.WriteString(shared.EscapeHTML(value))
	builder.WriteString("<br>")
	appendOptionalLineBreak(builder, "<br>", addEmptyLine)
}

func (htmlExecutionWriter) WriteOptions(builder *strings.Builder, label string, options []string) {
	builder.WriteString("<b>")
	builder.WriteString(shared.EscapeHTML(label))
	builder.WriteString(":</b><br>")
	for idx, option := range options {
		builder.WriteString(fmt.Sprintf("%d) %s<br>", idx+1, shared.EscapeHTML(option)))
	}
	builder.WriteString("<br>")
}

func (htmlExecutionWriter) WriteCodeValue(builder *strings.Builder, label, value string, addEmptyLine bool) {
	builder.WriteString("<b>")
	builder.WriteString(shared.EscapeHTML(label))
	builder.WriteString(":</b> <code>")
	builder.WriteString(shared.EscapeHTML(value))
	builder.WriteString("</code><br>")
	appendOptionalLineBreak(builder, "<br>", addEmptyLine)
}

func appendOptionalLineBreak(builder *strings.Builder, lineBreak string, enabled bool) {
	if enabled {
		builder.WriteString(lineBreak)
	}
}

type executionLabels struct {
	ContextTitle  string
	ActionTitle   string
	QuestionLabel string
	ContextLabel  string
	OptionsLabel  string
}

func executionLabelsFor(msg i18n.Messages) executionLabels {
	return executionLabels{
		ContextTitle:  fallbackText(msg.SectionContext, "Context"),
		ActionTitle:   fallbackText(msg.SectionAction, "Action"),
		QuestionLabel: fallbackText(msg.QuestionLabel, "Question"),
		ContextLabel:  fallbackText(msg.ContextLabel, "Context"),
		OptionsLabel:  fallbackText(msg.OptionsLabel, "Options"),
	}
}

func fallbackText(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
