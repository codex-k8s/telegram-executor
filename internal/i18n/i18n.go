package i18n

import (
	"embed"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Messages contains localized strings for the bot.
type Messages struct {
	ExecutionTitle       string `yaml:"execution_title"`
	ExecutionCorrelation string `yaml:"execution_correlation"`
	ExecutionTool        string `yaml:"execution_tool"`
	ExecutionParams      string `yaml:"execution_params"`
	SectionContext       string `yaml:"section_context"`
	SectionAction        string `yaml:"section_action"`
	SectionParams        string `yaml:"section_params"`
	QuestionLabel        string `yaml:"question_label"`
	ContextLabel         string `yaml:"context_label"`
	OptionsLabel         string `yaml:"options_label"`
	CustomOptionButton   string `yaml:"custom_option_button"`
	CancelCustomButton   string `yaml:"cancel_custom_button"`
	DeleteButton         string `yaml:"delete_button"`
	CustomPrompt         string `yaml:"custom_prompt"`
	SelectedNote         string `yaml:"selected_note"`
	TimeoutNote          string `yaml:"timeout_note"`
	ErrorNote            string `yaml:"error_note"`
	InvalidAction        string `yaml:"invalid_action"`
	AlreadyResolved      string `yaml:"already_resolved"`
	InvalidChat          string `yaml:"invalid_chat"`
	VoiceDisabled        string `yaml:"voice_disabled"`
	TranscriptionFailed  string `yaml:"transcription_failed"`
}

// Bundle combines language code and messages.
type Bundle struct {
	// Lang is the selected language.
	Lang string
	// Messages are localized strings.
	Messages Messages
}

//go:embed *.yaml
var files embed.FS

// Load loads i18n messages for the requested language.
func Load(lang string) (Bundle, error) {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if lang == "" {
		lang = "en"
	}

	messages, err := loadMessages(lang)
	if err != nil && lang != "en" {
		messages, err = loadMessages("en")
		if err != nil {
			return Bundle{}, err
		}
		lang = "en"
	} else if err != nil {
		return Bundle{}, err
	}

	return Bundle{Lang: lang, Messages: messages}, nil
}

func loadMessages(lang string) (Messages, error) {
	data, err := files.ReadFile(fmt.Sprintf("%s.yaml", lang))
	if err != nil {
		return Messages{}, err
	}
	var msg Messages
	if err := yaml.Unmarshal(data, &msg); err != nil {
		return Messages{}, err
	}
	return msg, nil
}
