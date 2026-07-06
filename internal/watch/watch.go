// Package watch implements a PM2-style dashboard daemon: it polls GitHub for
// PRs where your review is requested, reviews them automatically, and shows
// every job in a live table where reviews can be cancelled, removed, or
// retried mid-flight.
package watch

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"prreview/internal/ai"
	"prreview/internal/config"
	"prreview/internal/github"
)

// Options configures the watch daemon.
type Options struct {
	Interval    time.Duration
	AutoApprove bool // approve clean PRs automatically (opt-in)
	Concurrency int
	// IncludeTeams also picks up PRs where a team you belong to is
	// requested; default false = direct user requests only.
	IncludeTeams bool
	// Repos limits auto-review to these owner/name repos. Empty = all.
	Repos []string
}

type status int

const (
	statusQueued status = iota
	statusRunning
	statusDone
	statusFailed
	statusCanceled
	statusSkipped
)

type item struct {
	ref      github.PRRef
	status   status
	stage    string
	posted   string // review state that was posted
	findings int
	err      error
	cancel   context.CancelFunc
	canceled bool
	started  time.Time
	finished time.Time
}

// messages
type polledMsg struct {
	refs []github.PRRef
	err  error
}
type pollTickMsg struct{}
type workerStageMsg struct {
	key   string
	stage string
}
type workerDoneMsg struct {
	key      string
	posted   string
	findings int
	headOid  string
	skipped  bool
}
type workerFailMsg struct {
	key      string
	err      error
	canceled bool
}

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	hintStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	keyStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	okStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	warnStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	errStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	runStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	selRowStyle = lipgloss.NewStyle().Background(lipgloss.Color("236"))
)

// Model is the watch dashboard.
type Model struct {
	cfg     *config.Config
	opts    Options
	events  chan tea.Msg
	items   []*item
	sel     int
	seen    *Store
	user    string
	running int
	spin    spinner.Model
	pollErr error
	lastAt  time.Time
	width   int
}

// New creates the watch dashboard model.
func New(cfg *config.Config, opts Options) (*Model, error) {
	if opts.Interval <= 0 {
		opts.Interval = 15 * time.Second
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 2
	}
	seen, err := LoadStore()
	if err != nil {
		return nil, err
	}
	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	sp.Style = runStyle
	return &Model{
		cfg:    cfg,
		opts:   opts,
		events: make(chan tea.Msg, 64),
		seen:   seen,
		spin:   sp,
	}, nil
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, m.pollCmd(), listen(m.events), fetchUserCmd())
}

// listen forwards one worker event from the channel; every handler of a
// channel-delivered message must return listen again to keep exactly one
// outstanding listener.
func listen(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

type userMsg struct{ login string }

func fetchUserCmd() tea.Cmd {
	return func() tea.Msg {
		if err := github.CheckGH(); err != nil {
			return polledMsg{err: err}
		}
		login, _ := github.CurrentUser()
		return userMsg{login: login}
	}
}

func (m *Model) pollCmd() tea.Cmd {
	includeTeams, repos := m.opts.IncludeTeams, m.opts.Repos
	return func() tea.Msg {
		refs, err := github.SearchReviewRequested(includeTeams, repos)
		return polledMsg{refs: refs, err: err}
	}
}

func (m *Model) find(key string) *item {
	for _, it := range m.items {
		if it.ref.Key() == key {
			return it
		}
	}
	return nil
}

func (m *Model) startNextQueued() {
	for _, it := range m.items {
		if m.running >= m.opts.Concurrency {
			return
		}
		if it.status == statusQueued {
			m.startWorker(it)
		}
	}
}

func (m *Model) startWorker(it *item) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	it.cancel = cancel
	it.canceled = false
	it.status = statusRunning
	it.stage = "fetching PR…"
	it.started = time.Now()
	m.running++

	cfg := m.cfg
	autoApprove := m.opts.AutoApprove
	user := m.user
	events := m.events
	key := it.ref.Key()
	url := it.ref.URL
	seen := m.seen

	go func() {
		defer cancel()
		fail := func(err error) {
			events <- workerFailMsg{key: key, err: err, canceled: ctx.Err() != nil}
		}

		pr, err := github.FetchPR(url)
		if err != nil {
			fail(err)
			return
		}
		if pr.State != "OPEN" {
			events <- workerDoneMsg{key: key, skipped: true, headOid: pr.HeadOid}
			return
		}
		if seen.Get(key) == pr.HeadOid && pr.HeadOid != "" {
			events <- workerDoneMsg{key: key, skipped: true, headOid: pr.HeadOid}
			return
		}

		result, err := ai.Analyze(ctx, cfg, pr, func(stage string) {
			events <- workerStageMsg{key: key, stage: stage}
		})
		if err != nil {
			fail(err)
			return
		}
		if ctx.Err() != nil {
			fail(ctx.Err())
			return
		}

		events <- workerStageMsg{key: key, stage: "posting review…"}
		state := "COMMENT"
		if len(result.Findings) == 0 && autoApprove && user != "" && pr.Author != user {
			state = "APPROVE"
		}
		body := result.Summary + "\n\n---\n_Automated review by prreview watch._"
		if _, err := github.PostReview(pr, state, body, result.Comments()); err != nil {
			fail(err)
			return
		}
		events <- workerDoneMsg{key: key, posted: state, findings: len(result.Findings), headOid: pr.HeadOid}
	}()
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case userMsg:
		m.user = msg.login
		return m, nil

	case polledMsg:
		m.lastAt = time.Now()
		m.pollErr = msg.err
		if msg.err == nil {
			for _, ref := range msg.refs {
				key := ref.Key()
				if m.find(key) != nil || m.seen.Ignored(key) {
					continue
				}
				m.items = append(m.items, &item{ref: ref, status: statusQueued, stage: "queued"})
			}
			m.startNextQueued()
		}
		return m, tea.Tick(m.opts.Interval, func(time.Time) tea.Msg { return pollTickMsg{} })

	case pollTickMsg:
		return m, m.pollCmd()

	case workerStageMsg:
		if it := m.find(msg.key); it != nil && it.status == statusRunning {
			it.stage = msg.stage
		}
		return m, listen(m.events)

	case workerDoneMsg:
		if it := m.find(msg.key); it != nil {
			m.running--
			it.finished = time.Now()
			if msg.skipped {
				it.status = statusSkipped
				it.stage = "already reviewed / not open"
			} else {
				it.status = statusDone
				it.posted = msg.posted
				it.findings = msg.findings
			}
			if msg.headOid != "" {
				m.seen.Set(msg.key, msg.headOid)
			}
			m.startNextQueued()
		}
		return m, listen(m.events)

	case workerFailMsg:
		if it := m.find(msg.key); it != nil {
			m.running--
			it.finished = time.Now()
			if msg.canceled || it.canceled {
				it.status = statusCanceled
				it.stage = "canceled"
			} else {
				it.status = statusFailed
				it.err = msg.err
			}
			m.startNextQueued()
		}
		return m, listen(m.events)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		for _, it := range m.items {
			if it.cancel != nil && it.status == statusRunning {
				it.cancel()
			}
		}
		return m, tea.Quit

	case "up", "k":
		if m.sel > 0 {
			m.sel--
		}
	case "down", "j":
		if m.sel < len(m.items)-1 {
			m.sel++
		}

	case "x": // cancel a running review
		if it := m.selected(); it != nil && it.status == statusRunning && it.cancel != nil {
			it.canceled = true
			it.cancel()
		}

	case "d": // delete row; never auto-pick it up again
		if it := m.selected(); it != nil {
			if it.status == statusRunning && it.cancel != nil {
				it.canceled = true
				it.cancel()
			}
			m.seen.Ignore(it.ref.Key())
			m.items = removeItem(m.items, it)
			if m.sel >= len(m.items) && m.sel > 0 {
				m.sel--
			}
		}

	case "r": // retry / force re-review
		if it := m.selected(); it != nil &&
			(it.status == statusDone || it.status == statusFailed ||
				it.status == statusCanceled || it.status == statusSkipped) {
			m.seen.Clear(it.ref.Key())
			it.status = statusQueued
			it.stage = "queued"
			it.err = nil
			it.posted = ""
			m.startNextQueued()
		}

	case "o": // open in browser
		if it := m.selected(); it != nil {
			_ = exec.Command("open", it.ref.URL).Start()
		}
	}
	return m, nil
}

func (m *Model) selected() *item {
	if m.sel < 0 || m.sel >= len(m.items) {
		return nil
	}
	return m.items[m.sel]
}

func removeItem(items []*item, target *item) []*item {
	out := items[:0]
	for _, it := range items {
		if it != target {
			out = append(out, it)
		}
	}
	return out
}

func (it *item) statusCell(spin string) string {
	switch it.status {
	case statusQueued:
		return dimStyle.Render("○ queued  ")
	case statusRunning:
		return runStyle.Render(spin + " running ")
	case statusDone:
		return okStyle.Render("✓ done    ")
	case statusFailed:
		return errStyle.Render("✗ failed  ")
	case statusCanceled:
		return warnStyle.Render("■ canceled")
	default:
		return dimStyle.Render("- skipped ")
	}
}

func (it *item) detailCell() string {
	switch it.status {
	case statusRunning:
		return it.stage
	case statusDone:
		return fmt.Sprintf("%s · %d findings", it.posted, it.findings)
	case statusFailed:
		if it.err != nil {
			s := it.err.Error()
			if len(s) > 60 {
				s = s[:60] + "…"
			}
			return s
		}
		return "error"
	default:
		return it.stage
	}
}

func (it *item) ageCell() string {
	switch it.status {
	case statusRunning:
		return time.Since(it.started).Round(time.Second).String()
	case statusDone, statusFailed, statusCanceled, statusSkipped:
		if !it.finished.IsZero() && !it.started.IsZero() {
			return it.finished.Sub(it.started).Round(time.Second).String()
		}
	}
	return ""
}

func (m *Model) View() string {
	var b strings.Builder
	b.WriteString("\n" + titleStyle.Render("prreview watch") + "  ")
	b.WriteString(hintStyle.Render(fmt.Sprintf("polling every %s · model %s · depth %s",
		m.opts.Interval, m.cfg.Model, m.cfg.Depth)))
	if m.opts.AutoApprove {
		b.WriteString("  " + warnStyle.Render("[auto-approve]"))
	}
	b.WriteString("\n")
	if m.pollErr != nil {
		b.WriteString(errStyle.Render("poll error: ") + m.pollErr.Error() + "\n")
	} else if !m.lastAt.IsZero() {
		b.WriteString(hintStyle.Render(fmt.Sprintf("last poll %s\n", m.lastAt.Format("15:04:05"))))
	}
	b.WriteString("\n")

	if len(m.items) == 0 {
		b.WriteString(dimStyle.Render("  no review requests yet — waiting…") + "\n")
	} else {
		header := fmt.Sprintf("  %-10s  %-32s  %-40s  %s", "STATUS", "PR", "DETAIL", "TIME")
		b.WriteString(hintStyle.Render(header) + "\n")
		for i, it := range m.items {
			pr := fmt.Sprintf("%s#%d", it.ref.Repo, it.ref.Number)
			if len(pr) > 32 {
				pr = pr[:31] + "…"
			}
			detail := it.detailCell()
			if len(detail) > 40 {
				detail = detail[:39] + "…"
			}
			row := fmt.Sprintf("  %s  %-32s  %-40s  %s", it.statusCell(m.spin.View()), pr, detail, it.ageCell())
			if i == m.sel {
				row = selRowStyle.Render(row)
			}
			b.WriteString(row + "\n")
			if i == m.sel {
				title := it.ref.Title
				if len(title) > 70 {
					title = title[:69] + "…"
				}
				b.WriteString(dimStyle.Render("              "+title) + "\n")
			}
		}
	}

	b.WriteString("\n" + hintStyle.Render(strings.Join([]string{
		keyStyle.Render("↑↓") + " select",
		keyStyle.Render("x") + " cancel",
		keyStyle.Render("d") + " remove",
		keyStyle.Render("r") + " re-review",
		keyStyle.Render("o") + " open",
		keyStyle.Render("q") + " quit",
	}, "  ·  ")) + "\n")
	return b.String()
}
