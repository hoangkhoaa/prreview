package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"prreview/internal/config"
)

// field kinds
const (
	fieldChoice = iota
	fieldText
)

type formField struct {
	kind    int
	label   string
	hint    string
	choices []string // fieldChoice
	choice  int
	input   textinput.Model // fieldText
}

// ConfigModel is a Cursor-style settings form: pick provider/depth with
// arrow keys, type endpoint/key/model in text fields.
type ConfigModel struct {
	cfg    *config.Config
	fields []formField
	focus  int
	saved  bool
	err    error
}

func newText(placeholder, value string, password bool) textinput.Model {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.Prompt = ""
	ti.CharLimit = 512
	ti.Width = 52
	ti.SetValue(value)
	if password {
		ti.EchoMode = textinput.EchoPassword
		ti.EchoCharacter = '*'
	}
	return ti
}

func choiceIndex(choices []string, v string) int {
	for i, c := range choices {
		if c == v {
			return i
		}
	}
	return 0
}

// NewConfigForm builds the interactive config form pre-filled from cfg.
func NewConfigForm(cfg *config.Config) ConfigModel {
	providers := []string{"anthropic", "openai"}
	depths := []string{"fast", "standard", "max"}

	fields := []formField{
		{kind: fieldChoice, label: "Provider", choices: providers,
			choice: choiceIndex(providers, cfg.Provider),
			hint:   "anthropic = Claude API · openai = OpenAI-compatible (vLLM, Ollama, LiteLLM, DeepSeek, ...)"},
		{kind: fieldText, label: "Base URL", input: newText("(default endpoint — required for self-hosted)", cfg.BaseURL, false),
			hint: "e.g. https://llm.internal.example.com — leave empty for api.anthropic.com"},
		{kind: fieldText, label: "API key", input: newText("sk-...", "", true),
			hint: "stored 0600 in ~/.config/prreview/config.json — leave empty to keep current"},
		{kind: fieldText, label: "Model", input: newText(config.DefaultModel, cfg.Model, false),
			hint: "anthropic: claude-opus-4-8 · self-hosted: your served model name (e.g. deepseek-chat)"},
		{kind: fieldChoice, label: "Depth", choices: depths,
			choice: choiceIndex(depths, cfg.Depth),
			hint:   "fast = 1 pass · standard = 2 lenses + verify · max = 4 lenses + verify (best, most tokens)"},
	}
	m := ConfigModel{cfg: cfg, fields: fields}
	m.setFocus(0)
	return m
}

func (m *ConfigModel) setFocus(i int) {
	m.focus = i
	for idx := range m.fields {
		if m.fields[idx].kind == fieldText {
			if idx == i {
				m.fields[idx].input.Focus()
			} else {
				m.fields[idx].input.Blur()
			}
		}
	}
}

func (m ConfigModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m ConfigModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		f := &m.fields[m.focus]
		switch key.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "up", "shift+tab":
			m.setFocus((m.focus - 1 + len(m.fields)) % len(m.fields))
			return m, nil
		case "down", "tab":
			m.setFocus((m.focus + 1) % len(m.fields))
			return m, nil
		case "left", "right":
			if f.kind == fieldChoice {
				d := 1
				if key.String() == "left" {
					d = -1
				}
				f.choice = (f.choice + d + len(f.choices)) % len(f.choices)
				return m, nil
			}
		case "enter":
			if m.focus < len(m.fields)-1 {
				m.setFocus(m.focus + 1)
				return m, nil
			}
			m.apply()
			m.err = config.Save(m.cfg)
			m.saved = m.err == nil
			return m, tea.Quit
		}
	}

	var cmds []tea.Cmd
	for i := range m.fields {
		if m.fields[i].kind == fieldText {
			var cmd tea.Cmd
			m.fields[i].input, cmd = m.fields[i].input.Update(msg)
			cmds = append(cmds, cmd)
		}
	}
	return m, tea.Batch(cmds...)
}

func (m *ConfigModel) apply() {
	m.cfg.Provider = m.fields[0].choices[m.fields[0].choice]
	m.cfg.BaseURL = strings.TrimSpace(m.fields[1].input.Value())
	if v := strings.TrimSpace(m.fields[2].input.Value()); v != "" {
		m.cfg.APIKey = v
	}
	if v := strings.TrimSpace(m.fields[3].input.Value()); v != "" {
		m.cfg.Model = v
	}
	m.cfg.Depth = m.fields[4].choices[m.fields[4].choice]
}

func (m ConfigModel) View() string {
	var b strings.Builder
	b.WriteString("\n" + titleStyle.Render("prreview settings") + "\n")
	b.WriteString(hintStyle.Render(fmt.Sprintf("  current key: %s\n\n", config.MaskKey(m.cfg.APIKey))))

	for i, f := range m.fields {
		cursor := "  "
		label := labelStyle.Render(fmt.Sprintf("%-9s", f.label))
		if i == m.focus {
			cursor = keyStyle.Render("› ")
			label = titleStyle.Render(fmt.Sprintf("%-9s", f.label))
		}
		var value string
		if f.kind == fieldChoice {
			var opts []string
			for ci, c := range f.choices {
				if ci == f.choice {
					opts = append(opts, okStyle.Render("["+c+"]"))
				} else {
					opts = append(opts, hintStyle.Render(" "+c+" "))
				}
			}
			value = strings.Join(opts, " ")
		} else {
			value = f.input.View()
		}
		b.WriteString(cursor + label + " " + value + "\n")
		if i == m.focus {
			b.WriteString(hintStyle.Render("            "+f.hint) + "\n")
		}
	}

	b.WriteString("\n" + hintStyle.Render("  ↑↓ field · ←→ choose · enter next/save · esc cancel") + "\n")
	return b.String()
}

// Saved reports whether the form saved the config, and any save error.
func (m ConfigModel) Saved() (bool, error) {
	return m.saved, m.err
}
