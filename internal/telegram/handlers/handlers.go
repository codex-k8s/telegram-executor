package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex-k8s/telegram-executor/internal/executions"
	"github.com/codex-k8s/telegram-executor/internal/i18n"
	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

const (
	// ActionOption selects one predefined option.
	ActionOption = "option"
	// ActionCustom starts custom-answer flow.
	ActionCustom = "custom"
	// ActionCancelCustom cancels custom-answer prompt.
	ActionCancelCustom = "custom_cancel"
	// ActionDelete deletes a resolved message.
	ActionDelete = "delete"
)

// Handler processes Telegram updates and resolves executions.
type Handler struct {
	bot         *telego.Bot
	registry    *executions.Registry
	messages    map[string]i18n.Messages
	defaultLang string
	chatID      int64
	sttLang     string
	transcriber Transcriber
	log         *slog.Logger
}

// Transcriber converts audio to text.
type Transcriber interface {
	Transcribe(ctx context.Context, reader io.Reader, filename, contentType, language string) (string, error)
}

// NewHandler creates a new update handler.
func NewHandler(bot *telego.Bot, registry *executions.Registry, messages map[string]i18n.Messages, defaultLang string, chatID int64, sttLang string, transcriber Transcriber, log *slog.Logger) *Handler {
	return &Handler{
		bot:         bot,
		registry:    registry,
		messages:    messages,
		defaultLang: defaultLang,
		chatID:      chatID,
		sttLang:     sttLang,
		transcriber: transcriber,
		log:         log,
	}
}

// Run processes updates until context cancellation.
func (h *Handler) Run(ctx context.Context, updates <-chan telego.Update) {
	for {
		select {
		case <-ctx.Done():
			return
		case update, ok := <-updates:
			if !ok {
				return
			}
			h.HandleUpdate(ctx, update)
		}
	}
}

// HandleUpdate processes a single update.
func (h *Handler) HandleUpdate(ctx context.Context, update telego.Update) {
	if update.CallbackQuery != nil {
		h.handleCallback(ctx, update.CallbackQuery)
		return
	}
	if update.Message != nil {
		h.handleMessage(ctx, update.Message)
		return
	}
}

func (h *Handler) handleCallback(ctx context.Context, query *telego.CallbackQuery) {
	if query.Message == nil {
		return
	}
	if !h.allowedChat(query.Message.GetChat().ID) {
		_ = h.answerCallback(ctx, query, h.messageFor("").InvalidChat)
		return
	}
	action, payload := parseCallback(query.Data)

	switch action {
	case ActionOption:
		h.resolveOption(ctx, query, payload)
	case ActionCustom:
		h.startCustomPrompt(ctx, query, payload)
	case ActionCancelCustom:
		h.cancelCustomPrompt(ctx, query, payload)
	case ActionDelete:
		h.deleteMessage(ctx, query, payload)
	default:
		_ = h.answerCallback(ctx, query, h.messageFor("").InvalidAction)
	}
}

func (h *Handler) handleMessage(ctx context.Context, message *telego.Message) {
	if !h.allowedChat(message.Chat.ID) {
		return
	}
	exec, _ := h.registry.CurrentPrompt()
	if exec == nil || !exec.AwaitingText {
		return
	}
	if message.Text != "" {
		answer := strings.TrimSpace(message.Text)
		if answer == "" {
			return
		}
		exec, promptID, ok := h.registry.Resolve(exec.Request.CorrelationID)
		if !ok {
			return
		}
		if promptID > 0 {
			_ = h.DeleteMessage(ctx, promptID)
		}
		output := map[string]any{
			"question":        exec.Request.Question,
			"selected_option": answer,
			"selected_index":  nil,
			"custom":          true,
			"input_mode":      "text",
		}
		note := fmt.Sprintf("✅ %s: %s", h.messageFor(exec.Request.Lang).SelectedNote, answer)
		h.FinalizeExecution(ctx, exec, executions.Result{Status: executions.StatusSuccess, Output: output, Note: note}, "")
		return
	}
	if message.Voice != nil {
		answer, err := h.transcribeVoice(ctx, message.Voice)
		if err != nil {
			if errors.Is(err, errTranscriberDisabled) {
				_ = h.reply(ctx, h.messageFor(exec.Request.Lang).VoiceDisabled)
			} else {
				_ = h.reply(ctx, h.messageFor(exec.Request.Lang).TranscriptionFailed)
			}
			return
		}
		answer = strings.TrimSpace(answer)
		if answer == "" {
			return
		}
		exec, promptID, ok := h.registry.Resolve(exec.Request.CorrelationID)
		if !ok {
			return
		}
		if promptID > 0 {
			_ = h.DeleteMessage(ctx, promptID)
		}
		output := map[string]any{
			"question":        exec.Request.Question,
			"selected_option": answer,
			"selected_index":  nil,
			"custom":          true,
			"input_mode":      "voice",
		}
		note := fmt.Sprintf("✅ %s: %s", h.messageFor(exec.Request.Lang).SelectedNote, answer)
		h.FinalizeExecution(ctx, exec, executions.Result{Status: executions.StatusSuccess, Output: output, Note: note}, "")
		return
	}
}

func (h *Handler) transcribeVoice(ctx context.Context, voice *telego.Voice) (string, error) {
	if h.transcriber == nil {
		return "", errTranscriberDisabled
	}
	file, err := h.bot.GetFile(ctx, &telego.GetFileParams{FileID: voice.FileID})
	if err != nil {
		return "", err
	}
	audioURL := h.bot.FileDownloadURL(file.FilePath)
	data, err := tu.DownloadFile(audioURL)
	if err != nil {
		return "", err
	}
	normalized, mimeType, fileName, err := normalizeVoiceAudio(ctx, data, "", file.FilePath)
	if err != nil {
		return "", err
	}
	reader := bytes.NewReader(normalized)
	return h.transcriber.Transcribe(ctx, reader, fileName, mimeType, h.sttLang)
}

var errTranscriberDisabled = errors.New("transcriber disabled")

func (h *Handler) allowedChat(chatID int64) bool {
	return chatID == h.chatID
}

func (h *Handler) answerCallback(ctx context.Context, query *telego.CallbackQuery, text string) error {
	params := &telego.AnswerCallbackQueryParams{CallbackQueryID: query.ID}
	if strings.TrimSpace(text) != "" {
		params.Text = text
	}
	return h.bot.AnswerCallbackQuery(ctx, params)
}

func (h *Handler) reply(ctx context.Context, text string) error {
	_, err := h.bot.SendMessage(ctx, &telego.SendMessageParams{
		ChatID:    tu.ID(h.chatID),
		Text:      text,
		ParseMode: telego.ModeMarkdown,
	})
	return err
}

func (h *Handler) deleteMessage(ctx context.Context, query *telego.CallbackQuery, payload string) {
	messageID, err := strconv.Atoi(payload)
	if err != nil || messageID <= 0 {
		_ = h.answerCallback(ctx, query, h.messageFor("").InvalidAction)
		return
	}
	_ = h.DeleteMessage(ctx, messageID)
	_ = h.answerCallback(ctx, query, "")
}

// CallbackData builds callback data for an action.
func CallbackData(action, payload string) string {
	if payload == "" {
		return action
	}
	return action + ":" + payload
}

func parseCallback(data string) (string, string) {
	parts := strings.SplitN(data, ":", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

func parseOptionPayload(payload string) (string, int, error) {
	parts := strings.SplitN(payload, "|", 2)
	if len(parts) != 2 {
		return "", 0, errors.New("invalid option payload")
	}
	index, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, errors.New("invalid option payload")
	}
	return parts[0], index, nil
}

func (h *Handler) resolveOption(ctx context.Context, query *telego.CallbackQuery, payload string) {
	correlationID, optionIndex, err := parseOptionPayload(payload)
	if err != nil {
		_ = h.answerCallback(ctx, query, h.messageFor("").InvalidAction)
		return
	}

	exec := h.registry.Get(correlationID)
	if exec == nil {
		_ = h.answerCallback(ctx, query, h.messageFor("").AlreadyResolved)
		return
	}
	if optionIndex < 0 || optionIndex >= len(exec.Request.Options) {
		_ = h.answerCallback(ctx, query, h.messageFor(exec.Request.Lang).InvalidAction)
		return
	}

	exec, promptID, ok := h.registry.Resolve(correlationID)
	if !ok {
		_ = h.answerCallback(ctx, query, h.messageFor("").AlreadyResolved)
		return
	}
	if promptID > 0 {
		_ = h.DeleteMessage(ctx, promptID)
	}

	selected := exec.Request.Options[optionIndex]
	output := map[string]any{
		"question":        exec.Request.Question,
		"selected_option": selected,
		"selected_index":  optionIndex,
		"custom":          false,
		"input_mode":      "button",
	}
	msg := h.messageFor(exec.Request.Lang)
	note := fmt.Sprintf("✅ %s: %s", msg.SelectedNote, selected)
	h.FinalizeExecution(ctx, exec, executions.Result{Status: executions.StatusSuccess, Output: output, Note: note}, "")
	_ = h.answerCallback(ctx, query, note)
}

func (h *Handler) startCustomPrompt(ctx context.Context, query *telego.CallbackQuery, correlationID string) {
	exec := h.registry.Get(correlationID)
	if exec == nil {
		_ = h.answerCallback(ctx, query, h.messageFor("").AlreadyResolved)
		return
	}
	if !exec.Request.AllowCustom {
		_ = h.answerCallback(ctx, query, h.messageFor(exec.Request.Lang).InvalidAction)
		return
	}
	prevPromptID, ok := h.registry.StartCustomInput(correlationID)
	if !ok {
		_ = h.answerCallback(ctx, query, h.messageFor(exec.Request.Lang).AlreadyResolved)
		return
	}
	if prevPromptID > 0 {
		_ = h.DeleteMessage(ctx, prevPromptID)
	}
	msg := h.messageFor(exec.Request.Lang)
	prompt, err := h.bot.SendMessage(ctx, &telego.SendMessageParams{
		ChatID:    tu.ID(h.chatID),
		Text:      msg.CustomPrompt,
		ParseMode: parseMode(exec.Request.Markup),
		ReplyParameters: (&telego.ReplyParameters{
			MessageID: exec.MessageID,
		}).WithAllowSendingWithoutReply(),
		ReplyMarkup: h.promptKeyboard(exec.Request.Lang, exec.Request.CorrelationID),
	})
	if err != nil {
		h.log.Error("Failed to send custom prompt", "error", err)
		_ = h.answerCallback(ctx, query, msg.ErrorNote)
		return
	}
	h.registry.SetPromptMessage(correlationID, prompt.MessageID)
	_ = h.answerCallback(ctx, query, "")
}

func (h *Handler) cancelCustomPrompt(ctx context.Context, query *telego.CallbackQuery, correlationID string) {
	promptID := h.registry.ClearPrompt(correlationID)
	if promptID > 0 {
		_ = h.DeleteMessage(ctx, promptID)
	}
	_ = h.answerCallback(ctx, query, "")
}

// FinalizeExecution updates Telegram message and sends webhook callback.
func (h *Handler) FinalizeExecution(ctx context.Context, exec *executions.Execution, result executions.Result, timeoutMessage string) {
	msg := h.messageFor(exec.Request.Lang)
	note := h.noteForResult(msg, result, timeoutMessage)
	text := exec.MessageText
	if strings.TrimSpace(note) != "" {
		text = fmt.Sprintf("%s\n\n%s", exec.MessageText, note)
	}
	_, err := h.bot.EditMessageText(ctx, &telego.EditMessageTextParams{
		ChatID:      tu.ID(h.chatID),
		MessageID:   exec.MessageID,
		Text:        text,
		ParseMode:   parseMode(exec.Request.Markup),
		ReplyMarkup: h.resolvedKeyboard(exec.Request.Lang, exec.MessageID),
	})
	if err != nil {
		h.log.Error("Failed to update telegram message", "error", err)
	}
	h.sendWebhook(ctx, exec, result)
}

// DeleteMessage removes a Telegram message.
func (h *Handler) DeleteMessage(ctx context.Context, messageID int) error {
	if messageID <= 0 {
		return nil
	}
	return h.bot.DeleteMessage(ctx, &telego.DeleteMessageParams{
		ChatID:    tu.ID(h.chatID),
		MessageID: messageID,
	})
}

func (h *Handler) sendWebhook(ctx context.Context, exec *executions.Execution, result executions.Result) {
	if exec == nil {
		return
	}
	if strings.TrimSpace(exec.Request.Callback.URL) == "" {
		return
	}
	payload := map[string]any{
		"correlation_id": exec.Request.CorrelationID,
		"status":         string(result.Status),
		"result":         result.Output,
		"tool":           exec.Request.Tool.Name,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, exec.Request.Callback.URL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	if _, err := client.Do(req); err != nil {
		h.log.Error("Webhook delivery failed", "error", err, "correlation_id", exec.Request.CorrelationID)
	}
}

func (h *Handler) messageFor(lang string) i18n.Messages {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if lang == "" {
		lang = h.defaultLang
	}
	if msg, ok := h.messages[lang]; ok {
		return msg
	}
	if msg, ok := h.messages["en"]; ok {
		return msg
	}
	return i18n.Messages{}
}

func (h *Handler) noteForResult(msg i18n.Messages, result executions.Result, timeoutMessage string) string {
	switch result.Status {
	case executions.StatusSuccess:
		if strings.TrimSpace(result.Note) != "" {
			return result.Note
		}
		if result.Output != nil {
			return fmt.Sprintf("✅ %v", result.Output)
		}
		return "✅ " + msg.SelectedNote
	case executions.StatusError:
		if value, ok := result.Output.(string); ok {
			if strings.TrimSpace(value) == "execution timeout" {
				if strings.TrimSpace(timeoutMessage) != "" {
					return timeoutMessage
				}
				return "⏱️ " + msg.TimeoutNote
			}
			if strings.TrimSpace(value) != "" {
				return "⚠️ " + value
			}
		}
		if strings.TrimSpace(result.Note) != "" {
			return result.Note
		}
		return "⚠️ " + msg.ErrorNote
	default:
		return ""
	}
}

func (h *Handler) promptKeyboard(lang, correlationID string) *telego.InlineKeyboardMarkup {
	msg := h.messageFor(lang)
	cancel := CallbackData(ActionCancelCustom, correlationID)
	return tu.InlineKeyboard(
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(msg.CancelCustomButton).WithCallbackData(cancel),
		),
	)
}

func (h *Handler) resolvedKeyboard(lang string, messageID int) *telego.InlineKeyboardMarkup {
	msg := h.messageFor(lang)
	del := CallbackData(ActionDelete, strconv.Itoa(messageID))
	return tu.InlineKeyboard(
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(msg.DeleteButton).WithCallbackData(del),
		),
	)
}

func parseMode(markup string) string {
	switch strings.ToLower(strings.TrimSpace(markup)) {
	case "html":
		return telego.ModeHTML
	default:
		return telego.ModeMarkdownV2
	}
}
