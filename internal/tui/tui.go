// Package tui implements the interactive terminal UI for reviewing and
// approving pull requests.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"prreview/internal/ai"
	"prreview/internal/config"
	"prreview/internal/github"
)

// Mode is the requested workflow, mirroring the skill's intent table.
type Mode string

const (
	ModeReview           Mode = "review"
	ModeApprove          Mode = "approve"
	ModeReviewAndApprove Mode = "review-and-approve"
)

type state int

const (
	stateFetching state = iota
	stateAnalyzing
	stateResults
	stateConfirm
	statePosting
	stateDone
	stateError
)

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	headerStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("62")).Padding(0, 1)
	labelStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	valueStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	addStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	delStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	criticalStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	majorStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("208"))
	minorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	nitStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	okStyle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	errStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	hintStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	keyStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	warnStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	pathStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("81"))
)

type fetchedMsg struct {
	pr   *github.PR
	user string
}
type analyzedMsg struct{ result *ai.Result }
type progressMsg string
type postedMsg struct {
	state        string
	numComments  int
	usedFallback bool
}
type errMsg struct{ err error }

// Model is the Bubble Tea model for a review session.
type Model struct {
	cfg  *config.Config
	url  string
	mode Mode

	state       state
	spinner     spinner.Model
	viewport    viewport.Model
	vpReady     bool
	width       int
	height      int
	pr          *github.PR
	currentUser string
	ownPR       bool
	result      *ai.Result
	pending     string // review state awaiting confirmation
	posted      postedMsg
	err         error
	startedAt   time.Time
	status      string       // current pipeline stage
	analysisCh  chan tea.Msg // progress + result stream from the pipeline
}

// New creates a review session model.
func New(cfg *config.Config, url string, mode Mode) Model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("212"))
	return Model{
		cfg:       cfg,
		url:       url,
		mode:      mode,
		state:     stateFetching,
		spinner:   sp,
		startedAt: time.Now(),
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, fetchCmd(m.url))
}

func fetchCmd(url string) tea.Cmd {
	return func() tea.Msg {
		if err := github.CheckGH(); err != nil {
			return errMsg{err}
		}
		pr, err := github.FetchPR(url)
		if err != nil {
			return errMsg{err}
		}
		if pr.State != "OPEN" {
			return errMsg{fmt.Errorf("PR #%d is %s — nothing to review", pr.Number, strings.ToLower(pr.State))}
		}
		user, err := github.CurrentUser()
		if err != nil {
			return errMsg{err}
		}
		return fetchedMsg{pr: pr, user: user}
	}
}

// startAnalysis launches the pipeline in a goroutine; stage updates and the
// final result arrive on the returned channel.
func startAnalysis(cfg *config.Config, pr *github.PR) chan tea.Msg {
	ch := make(chan tea.Msg, 16)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		result, err := ai.Analyze(ctx, cfg, pr, func(stage string) {
			ch <- progressMsg(stage)
		})
		if err != nil {
			ch <- errMsg{err}
			return
		}
		ch <- analyzedMsg{result: result}
	}()
	return ch
}

func listen(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

func postCmd(pr *github.PR, reviewState, body string, comments []github.ReviewComment) tea.Cmd {
	return func() tea.Msg {
		fallback, err := github.PostReview(pr, reviewState, body, comments)
		if err != nil {
			return errMsg{err}
		}
		return postedMsg{state: reviewState, numComments: len(comments), usedFallback: fallback}
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		vpHeight := msg.Height - 14
		if vpHeight < 5 {
			vpHeight = 5
		}
		if !m.vpReady {
			m.viewport = viewport.New(msg.Width-2, vpHeight)
			m.vpReady = true
		} else {
			m.viewport.Width = msg.Width - 2
			m.viewport.Height = vpHeight
		}
		if m.result != nil {
			m.viewport.SetContent(m.renderFindings())
		}
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case fetchedMsg:
		m.pr = msg.pr
		m.currentUser = msg.user
		m.ownPR = msg.pr.Author == msg.user
		m.state = stateAnalyzing
		m.status = "starting review pipeline…"
		m.analysisCh = startAnalysis(m.cfg, m.pr)
		return m, listen(m.analysisCh)

	case progressMsg:
		m.status = string(msg)
		return m, listen(m.analysisCh)

	case analyzedMsg:
		m.result = msg.result
		m.state = stateResults
		if m.vpReady {
			m.viewport.SetContent(m.renderFindings())
		}
		return m, nil

	case postedMsg:
		m.posted = msg
		m.state = stateDone
		return m, nil

	case errMsg:
		m.err = msg.err
		m.state = stateError
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	if key == "ctrl+c" {
		return m, tea.Quit
	}

	switch m.state {
	case stateResults:
		switch key {
		case "q", "esc":
			return m, tea.Quit
		case "c":
			m.state = statePosting
			return m, tea.Batch(m.spinner.Tick, postCmd(m.pr, "COMMENT", m.reviewBody("COMMENT"), m.result.Comments()))
		case "a":
			if m.ownPR {
				return m, nil // cannot approve own PR
			}
			if len(m.result.Findings) > 0 {
				// Skill safety rule: confirm before approving despite issues.
				m.pending = "APPROVE"
				m.state = stateConfirm
				return m, nil
			}
			m.state = statePosting
			return m, tea.Batch(m.spinner.Tick, postCmd(m.pr, "APPROVE", m.reviewBody("APPROVE"), nil))
		case "r":
			if m.ownPR {
				return m, nil
			}
			m.state = statePosting
			return m, tea.Batch(m.spinner.Tick, postCmd(m.pr, "REQUEST_CHANGES", m.reviewBody("REQUEST_CHANGES"), m.result.Comments()))
		default:
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}

	case stateConfirm:
		switch key {
		case "y", "Y":
			m.state = statePosting
			return m, tea.Batch(m.spinner.Tick, postCmd(m.pr, m.pending, m.reviewBody(m.pending), m.result.Comments()))
		case "n", "N", "esc", "q":
			m.pending = ""
			m.state = stateResults
			return m, nil
		}

	case stateDone, stateError:
		return m, tea.Quit
	}

	return m, nil
}

// recommended returns the review state the skill's decision table suggests.
func (m Model) recommended() string {
	if m.ownPR {
		return "COMMENT"
	}
	hasIssues := len(m.result.Findings) > 0
	switch m.mode {
	case ModeApprove:
		return "APPROVE"
	case ModeReview:
		if hasIssues {
			return "REQUEST_CHANGES"
		}
		return "COMMENT"
	default: // review-and-approve
		if hasIssues {
			return "REQUEST_CHANGES"
		}
		return "APPROVE"
	}
}

func (m Model) reviewBody(reviewState string) string {
	var b strings.Builder
	b.WriteString(m.result.Summary)
	if reviewState == "APPROVE" && b.Len() == 0 {
		// Skill safety rule: never post an empty approval body.
		b.WriteString("LGTM — reviewed with prreview.")
	}
	b.WriteString("\n\n---\n_Reviewed with [prreview](https://github.com/) — AI-assisted code review._")
	return b.String()
}

func severityStyle(sev string) lipgloss.Style {
	switch sev {
	case "critical":
		return criticalStyle
	case "major":
		return majorStyle
	case "minor":
		return minorStyle
	default:
		return nitStyle
	}
}

func (m Model) renderFindings() string {
	var b strings.Builder
	b.WriteString(valueStyle.Render(strings.TrimSpace(m.result.Summary)))
	b.WriteString("\n\n")
	stats := fmt.Sprintf("%d files of context · %d candidates → %d confirmed",
		m.result.FilesFetched, m.result.Candidates, len(m.result.Findings))
	b.WriteString(hintStyle.Render(stats) + "\n\n")
	if len(m.result.Findings) == 0 {
		b.WriteString(okStyle.Render("✓ No issues confirmed — the diff looks clean."))
		b.WriteString("\n")
		return b.String()
	}
	b.WriteString(titleStyle.Render(fmt.Sprintf("Findings (%d)", len(m.result.Findings))))
	b.WriteString("\n\n")
	for i, f := range m.result.Findings {
		sev := severityStyle(f.Severity).Render(strings.ToUpper(f.Severity))
		loc := pathStyle.Render(fmt.Sprintf("%s:%d", f.Path, f.Line))
		conf := hintStyle.Render(fmt.Sprintf("(%d%%)", f.Confidence))
		fmt.Fprintf(&b, "%2d. %s  %s  %s\n", i+1, sev, loc, conf)
		b.WriteString("    " + valueStyle.Render(f.Comment) + "\n\n")
	}
	return b.String()
}

func (m Model) headerView() string {
	if m.pr == nil {
		return ""
	}
	stats := fmt.Sprintf("%s %s  %s files",
		addStyle.Render(fmt.Sprintf("+%d", m.pr.Additions)),
		delStyle.Render(fmt.Sprintf("-%d", m.pr.Deletions)),
		valueStyle.Render(fmt.Sprintf("%d", m.pr.ChangedFiles)),
	)
	own := ""
	if m.ownPR {
		own = warnStyle.Render("  [your own PR — comment only]")
	}
	lines := []string{
		titleStyle.Render(fmt.Sprintf("#%d %s", m.pr.Number, m.pr.Title)),
		labelStyle.Render("repo   ") + valueStyle.Render(fmt.Sprintf("%s/%s", m.pr.Owner, m.pr.Repo)),
		labelStyle.Render("author ") + valueStyle.Render(m.pr.Author) + own,
		labelStyle.Render("branch ") + valueStyle.Render(fmt.Sprintf("%s → %s", m.pr.HeadRef, m.pr.BaseRef)),
		labelStyle.Render("diff   ") + stats,
	}
	return headerStyle.Render(strings.Join(lines, "\n"))
}

func (m Model) footerView() string {
	rec := m.recommended()
	var keys []string
	if !m.ownPR {
		keys = append(keys, keyStyle.Render("a")+" approve")
		keys = append(keys, keyStyle.Render("r")+" request changes")
	}
	keys = append(keys, keyStyle.Render("c")+" comment")
	keys = append(keys, keyStyle.Render("↑/↓")+" scroll")
	keys = append(keys, keyStyle.Render("q")+" quit")
	return hintStyle.Render("recommended: ") + okStyle.Render(rec) + "\n" + hintStyle.Render(strings.Join(keys, "  ·  "))
}

func (m Model) View() string {
	switch m.state {
	case stateFetching:
		return fmt.Sprintf("\n %s Fetching PR %s ...\n", m.spinner.View(), m.url)

	case stateAnalyzing:
		return fmt.Sprintf("\n%s\n\n %s %s\n   %s\n",
			m.headerView(), m.spinner.View(),
			fmt.Sprintf("Reviewing with %s (depth: %s)", m.cfg.Model, m.cfg.Depth),
			hintStyle.Render(m.status))

	case stateResults:
		return fmt.Sprintf("\n%s\n\n%s\n\n%s\n", m.headerView(), m.viewport.View(), m.footerView())

	case stateConfirm:
		return fmt.Sprintf("\n%s\n\n%s\n\n%s\n",
			m.headerView(),
			warnStyle.Render(fmt.Sprintf("⚠ %d finding(s) were reported. Approve anyway?", len(m.result.Findings))),
			hintStyle.Render(keyStyle.Render("y")+" yes, approve  ·  "+keyStyle.Render("n")+" no, go back"))

	case statePosting:
		return fmt.Sprintf("\n%s\n\n %s Posting review to GitHub ...\n", m.headerView(), m.spinner.View())

	case stateDone:
		var msg string
		switch m.posted.state {
		case "APPROVE":
			msg = okStyle.Render(fmt.Sprintf("✓ Approved #%d.", m.pr.Number))
		case "REQUEST_CHANGES":
			msg = warnStyle.Render(fmt.Sprintf("✗ Requested changes on #%d (%d line comments).", m.pr.Number, m.posted.numComments))
		default:
			msg = okStyle.Render(fmt.Sprintf("✓ Posted comment review on #%d (%d line comments).", m.pr.Number, m.posted.numComments))
			if m.ownPR {
				msg += hintStyle.Render("\n  Note: you can't approve your own PR; a teammate needs to approve.")
			}
		}
		if m.posted.usedFallback {
			msg += hintStyle.Render("\n  Some line comments didn't match the diff; findings were folded into the review body.")
		}
		return fmt.Sprintf("\n%s\n\n%s\n\n%s\n", m.headerView(), msg, hintStyle.Render("press any key to exit"))

	case stateError:
		return fmt.Sprintf("\n%s %v\n\n%s\n", errStyle.Render("error:"), m.err, hintStyle.Render("press any key to exit"))
	}
	return ""
}
