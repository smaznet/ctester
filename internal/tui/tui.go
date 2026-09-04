package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aria/x-tester/internal/activity"
	"github.com/aria/x-tester/internal/balancer"
	"github.com/aria/x-tester/internal/logbuf"
	"github.com/aria/x-tester/internal/metrics"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type StatusFunc func() Status

type Status struct {
	Mode       string
	Balancer   string
	Listen     string
	StatsAddr  string
	TotalNodes int
	Active     int
	Standby    int
	Failed     int
	Ignored    int
	Pending    int
	Countries  map[string]int
	Filter     []string
	Traffic    metrics.Snapshot
	Nodes      []balancer.NodeState
	Logs       []string
	Activity   activity.Snapshot
}

type model struct {
	status   StatusFunc
	logs     *logbuf.Ring
	width    int
	height   int
	data     Status
	ready    bool
	quitting bool
}

type tickMsg time.Time

func New(status StatusFunc, logs *logbuf.Ring) *tea.Program {
	m := model{status: status, logs: logs, width: 80, height: 24}
	return tea.NewProgram(m,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(), // capture mouse wheel so terminal doesn't scroll
	)
}

func (m model) Init() tea.Cmd { return tick() }

func tick() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		}
	case tea.MouseMsg:
		// swallow wheel events — keep screen fixed
		return m, nil
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
	case tickMsg:
		if m.status != nil {
			m.data = m.status()
		}
		return m, tick()
	}
	return m, nil
}

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	labelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	badStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	valStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Bold(true)
	mutedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	actStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	sepStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

func (m model) View() string {
	h := m.height
	w := m.width
	if h < 10 {
		h = 10
	}
	if w < 40 {
		w = 40
	}
	if !m.ready {
		return "loading…"
	}

	d := m.data

	// Fixed layout budgets (exact line counts, no borders that grow unpredictably)
	const (
		headerLines = 9 // title + stats + geo + activity
		sepLines    = 2
		footerHint  = 1
	)
	logLinesN := 8
	if h >= 36 {
		logLinesN = 12
	}
	if h < 22 {
		logLinesN = 5
	}
	nodesBudget := h - headerLines - sepLines - logLinesN - footerHint - 1 // -1 for "nodes" title
	if nodesBudget < 2 {
		nodesBudget = 2
		logLinesN = h - headerLines - sepLines - footerHint - 1 - nodesBudget
		if logLinesN < 3 {
			logLinesN = 3
		}
	}

	var lines []string
	lines = append(lines, titleStyle.Render("x-tester")+"  "+mutedStyle.Render("fixed tui · q quit"))
	lines = append(lines, fmt.Sprintf("%s %s  %s %s  %s %s",
		labelStyle.Render("mode"), valStyle.Render(clip(d.Mode, 10)),
		labelStyle.Render("bal"), valStyle.Render(clip(d.Balancer, 16)),
		labelStyle.Render("listen"), valStyle.Render(clip(d.Listen, w-40)),
	))
	lines = append(lines, fmt.Sprintf("%s %s  %s %s  %s %s  %s %s  %s %s  %s %s",
		labelStyle.Render("nodes"), valStyle.Render(fmt.Sprintf("%d", d.TotalNodes)),
		labelStyle.Render("active"), okStyle.Render(fmt.Sprintf("%d", d.Active)),
		labelStyle.Render("standby"), valStyle.Render(fmt.Sprintf("%d", d.Standby)),
		labelStyle.Render("failed"), badStyle.Render(fmt.Sprintf("%d", d.Failed)),
		labelStyle.Render("ign"), mutedStyle.Render(fmt.Sprintf("%d", d.Ignored)),
		labelStyle.Render("conns"), valStyle.Render(fmt.Sprintf("%d", d.Traffic.Conns)),
	))
	// country groups + filter
	var gparts []string
	if len(d.Filter) > 0 {
		gparts = append(gparts, "filter="+strings.Join(d.Filter, ","))
	}
	if len(d.Countries) > 0 {
		type kv struct {
			k string
			v int
		}
		var list []kv
		for k, v := range d.Countries {
			list = append(list, kv{k, v})
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].v == list[j].v {
				return list[i].k < list[j].k
			}
			return list[i].v > list[j].v
		})
		for i, x := range list {
			if i >= 8 {
				gparts = append(gparts, fmt.Sprintf("+%d", len(list)-8))
				break
			}
			gparts = append(gparts, fmt.Sprintf("%s:%d", x.k, x.v))
		}
	}
	if len(gparts) == 0 {
		gparts = append(gparts, "no country data yet")
	}
	lines = append(lines, labelStyle.Render("geo ")+valStyle.Render(clip(strings.Join(gparts, "  "), w-5)))
	lines = append(lines, fmt.Sprintf("%s %s  %s %s",
		labelStyle.Render("↓in"), valStyle.Render(formatRate(d.Traffic.DownRate)),
		labelStyle.Render("↑out"), valStyle.Render(formatRate(d.Traffic.UpRate)),
	))
	lines = append(lines, fmt.Sprintf("%s %s  %s %s  %s %s",
		labelStyle.Render("↓tot"), mutedStyle.Render(formatBytes(d.Traffic.BytesDown)),
		labelStyle.Render("↑tot"), mutedStyle.Render(formatBytes(d.Traffic.BytesUp)),
		labelStyle.Render("stats"), mutedStyle.Render(clip(d.StatsAddr, 22)),
	))
	lines = append(lines, actStyle.Render("▸ "+clip(d.Activity.Line, w-3)))
	if d.Activity.LastOK != "" || d.Activity.LastFail != "" {
		lines = append(lines, mutedStyle.Render(clip(
			"  last ✓ "+d.Activity.LastOK+"  ✗ "+d.Activity.LastFail, w-1)))
	} else {
		lines = append(lines, "")
	}

	lines = append(lines, sepStyle.Render(strings.Repeat("─", w)))
	lines = append(lines, labelStyle.Render("nodes"))

	nodes := d.Nodes
	if len(nodes) == 0 {
		lines = append(lines, mutedStyle.Render("  waiting for subscription…"))
		for i := 1; i < nodesBudget; i++ {
			lines = append(lines, "")
		}
	} else {
		shown := 0
		for _, n := range nodes {
			if shown >= nodesBudget {
				break
			}
			mark := "·"
			style := mutedStyle
			if n.Active && !n.Failed {
				mark = "✓"
				style = okStyle
			} else if n.Failed {
				mark = "✗"
				style = badStyle
			}
			lat := "-"
			if n.Latency > 0 {
				lat = n.Latency.Round(time.Millisecond).String()
			}
			name := n.Name
			if name == "" {
				name = n.ID
			}
			row := fmt.Sprintf(" %s %-18s %4s %8s  %s", mark, clip(name, 18), clip(n.Country, 4), lat, clip(n.Address, w-38))
			lines = append(lines, style.Render(clip(row, w)))
			shown++
		}
		for shown < nodesBudget {
			lines = append(lines, "")
			shown++
		}
	}

	lines = append(lines, sepStyle.Render(strings.Repeat("─", w)))
	lines = append(lines, labelStyle.Render("log"))

	logLines := d.Logs
	if len(logLines) == 0 && m.logs != nil {
		logLines = m.logs.Lines(logLinesN)
	}
	if len(logLines) > logLinesN {
		logLines = logLines[len(logLines)-logLinesN:]
	}
	// pad to fixed log height
	pad := logLinesN - len(logLines)
	for i := 0; i < pad; i++ {
		lines = append(lines, "")
	}
	for _, l := range logLines {
		lines = append(lines, mutedStyle.Render(clip(l, w)))
	}

	// Exact height: truncate or pad
	out := make([]string, h)
	for i := 0; i < h; i++ {
		if i < len(lines) {
			out[i] = clip(lines[i], w)
		} else {
			out[i] = ""
		}
	}
	return strings.Join(out, "\n")
}

func clip(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}

func formatRate(bps float64) string {
	switch {
	case bps < 1024:
		return fmt.Sprintf("%6.0f B/s", bps)
	case bps < 1024*1024:
		return fmt.Sprintf("%6.1f KB/s", bps/1024)
	case bps < 1024*1024*1024:
		return fmt.Sprintf("%6.2f MB/s", bps/(1024*1024))
	default:
		return fmt.Sprintf("%6.2f GB/s", bps/(1024*1024*1024))
	}
}

func formatBytes(n uint64) string {
	const kb, mb, gb = 1024, 1024 * 1024, 1024 * 1024 * 1024
	switch {
	case n >= gb:
		return fmt.Sprintf("%.2f GB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.2f MB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
