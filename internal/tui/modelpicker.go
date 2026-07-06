package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"prreview/internal/config"
)

// ModelPicker is a Cursor-style model manager: a list of named model
// profiles where enter sets the default, plus an inline add/edit form.
type ModelPicker struct {
	cfg    *config.Config
	names  []string
	sel    int
	mode     string // "list" | "form"
	form     []formField
	focusIdx int
	editOf   string // profile being edited ("" = adding new)
	saved    bool
	err      error
	notice   string
}

// NewModelPicker builds the model manager UI.
func NewModelPicker(cfg *config.Config) ModelPicker {
	m := ModelPicker{cfg: cfg, mode: "list"}
	m.reload()
	return m
}

func (m *ModelPicker) reload() {
	m.names = m.names[:0]
	for n := range m.cfg.Models {
		m.names = append(m.names, n)
	}
	sort.Strings(m.names)
	if m.sel >= len(m.names) {
		m.sel = max(0, len(m.names)-1)
	}
}

func nameInput(value string) textinput.Model {
	ti := newText("e.g. deepseek, claude, local", value, false)
	ti.CharLimit = 32
	return ti
}

func (m *ModelPicker) openForm(editName string) {
	m.editOf = editName
	var p config.ModelProfile
	if editName != "" {
		p = m.cfg.Models[editName]
	} else {
		p = config.ModelProfile{Provider: "openai"}
	}
	providers := []string{"anthropic", "openai"}
	m.form = []formField{
		{kind: fieldText, label: "Name", input: nameInput(editName),
			hint: "short name you'll use to switch, e.g. deepseek"},
		{kind: fieldChoice, label: "Provider", choices: providers,
			choice: choiceIndex(providers, p.Provider),
			hint:   "anthropic = Claude API · openai = OpenAI-compatible (vLLM/Ollama/LiteLLM/DeepSeek)"},
		{kind: fieldText, label: "Base URL", input: newText("(empty = provider default)", p.BaseURL, false),
			hint: "your endpoint, e.g. https://llm.internal.example.com"},
		{kind: fieldText, label: "API key", input: newText("(empty = reuse global key)", "", true),
			hint: "per-profile key; leave empty to reuse the global api_key"},
		{kind: fieldText, label: "Model ID", input: newText("deepseek-chat", p.Model, false),
			hint: "model name as served by the endpoint"},
	}
	m.mode = "form"
	m.focusForm(0)
}

func (m *ModelPicker) focusForm(i int) {
	for idx := range m.form {
		if m.form[idx].kind == fieldText {
			if idx == i {
				m.form[idx].input.Focus()
			} else {
				m.form[idx].input.Blur()
			}
		}
	}
	m.focusIdx = i
}

func (m ModelPicker) Init() tea.Cmd { return textinput.Blink }

func (m ModelPicker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m.updateFormInputs(msg)
	}

	if m.mode == "form" {
		return m.updateForm(key)
	}

	switch key.String() {
	case "ctrl+c", "q", "esc":
		return m, tea.Quit
	case "up", "k":
		if m.sel > 0 {
			m.sel--
		}
	case "down", "j":
		if m.sel < len(m.names)-1 {
			m.sel++
		}
	case "enter":
		if len(m.names) > 0 {
			m.cfg.DefaultModelName = m.names[m.sel]
			m.err = config.Save(m.cfg)
			m.saved = m.err == nil
			m.notice = fmt.Sprintf("default model → %s", m.names[m.sel])
		}
	case "a":
		m.openForm("")
	case "e":
		if len(m.names) > 0 {
			m.openForm(m.names[m.sel])
		}
	case "d":
		if len(m.names) > 0 {
			name := m.names[m.sel]
			delete(m.cfg.Models, name)
			if m.cfg.DefaultModelName == name {
				m.cfg.DefaultModelName = ""
			}
			m.err = config.Save(m.cfg)
			m.notice = fmt.Sprintf("removed %s", name)
			m.reload()
		}
	}
	return m, nil
}

func (m ModelPicker) updateForm(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	f := &m.form[m.focusIdx]
	switch key.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.mode = "list"
		return m, nil
	case "up", "shift+tab":
		m.focusForm((m.focusIdx - 1 + len(m.form)) % len(m.form))
		return m, nil
	case "down", "tab":
		m.focusForm((m.focusIdx + 1) % len(m.form))
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
		if m.focusIdx < len(m.form)-1 {
			m.focusForm(m.focusIdx + 1)
			return m, nil
		}
		return m.submitForm()
	}
	return m.updateFormInputs(key)
}

func (m ModelPicker) updateFormInputs(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.mode != "form" {
		return m, nil
	}
	var cmds []tea.Cmd
	for i := range m.form {
		if m.form[i].kind == fieldText {
			var cmd tea.Cmd
			m.form[i].input, cmd = m.form[i].input.Update(msg)
			cmds = append(cmds, cmd)
		}
	}
	return m, tea.Batch(cmds...)
}

func (m ModelPicker) submitForm() (tea.Model, tea.Cmd) {
	name := strings.TrimSpace(m.form[0].input.Value())
	modelID := strings.TrimSpace(m.form[4].input.Value())
	if name == "" || modelID == "" {
		m.notice = "name and model ID are required"
		return m, nil
	}
	p := config.ModelProfile{
		Provider: m.form[1].choices[m.form[1].choice],
		BaseURL:  strings.TrimSpace(m.form[2].input.Value()),
		Model:    modelID,
	}
	if v := strings.TrimSpace(m.form[3].input.Value()); v != "" {
		p.APIKey = v
	} else if m.editOf != "" {
		p.APIKey = m.cfg.Models[m.editOf].APIKey // keep existing key
	}
	if m.cfg.Models == nil {
		m.cfg.Models = map[string]config.ModelProfile{}
	}
	if m.editOf != "" && m.editOf != name {
		delete(m.cfg.Models, m.editOf)
		if m.cfg.DefaultModelName == m.editOf {
			m.cfg.DefaultModelName = name
		}
	}
	m.cfg.Models[name] = p
	if m.cfg.DefaultModelName == "" {
		m.cfg.DefaultModelName = name
	}
	m.err = config.Save(m.cfg)
	m.saved = m.err == nil
	m.notice = fmt.Sprintf("saved %s", name)
	m.mode = "list"
	m.reload()
	for i, n := range m.names {
		if n == name {
			m.sel = i
		}
	}
	return m, nil
}

func (m ModelPicker) View() string {
	var b strings.Builder
	b.WriteString("\n" + titleStyle.Render("prreview models") + "\n\n")

	if m.mode == "form" {
		action := "add model"
		if m.editOf != "" {
			action = "edit " + m.editOf
		}
		b.WriteString("  " + warnStyle.Render(action) + "\n\n")
		for i, f := range m.form {
			cursor := "  "
			label := labelStyle.Render(fmt.Sprintf("%-9s", f.label))
			if i == m.focusIdx {
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
			if i == m.focusIdx {
				b.WriteString(hintStyle.Render("            "+f.hint) + "\n")
			}
		}
		b.WriteString("\n" + hintStyle.Render("  ↑↓ field · ←→ choose · enter next/save · esc back") + "\n")
		return b.String()
	}

	if len(m.names) == 0 {
		b.WriteString(hintStyle.Render("  no model profiles yet — press ") +
			keyStyle.Render("a") + hintStyle.Render(" to add one") + "\n")
	}
	for i, n := range m.names {
		p := m.cfg.Models[n]
		cursor := "  "
		if i == m.sel {
			cursor = keyStyle.Render("› ")
		}
		def := "  "
		if n == m.cfg.DefaultModelName {
			def = okStyle.Render("● ")
		}
		endpoint := p.BaseURL
		if endpoint == "" {
			endpoint = "(default endpoint)"
		}
		line := fmt.Sprintf("%s%s%-14s %-10s %-28s %s", cursor, def, n, p.Provider, p.Model, hintStyle.Render(endpoint))
		b.WriteString(line + "\n")
	}
	if m.notice != "" {
		b.WriteString("\n  " + okStyle.Render(m.notice) + "\n")
	}
	if m.err != nil {
		b.WriteString("\n  " + errStyle.Render("error: "+m.err.Error()) + "\n")
	}
	b.WriteString("\n" + hintStyle.Render(strings.Join([]string{
		keyStyle.Render("enter") + " set default",
		keyStyle.Render("a") + " add",
		keyStyle.Render("e") + " edit",
		keyStyle.Render("d") + " delete",
		keyStyle.Render("q") + " quit",
	}, "  ·  ")) + "\n")
	return b.String()
}
