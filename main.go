// imtui — a minimal Immich client for the terminal.
//
// Browse recent photos, search by context (CLIP), by OCR'd text, or BY an
// image; browse and rename people; edit photo descriptions. Previews render
// through chafa.
//
// Keys: j/k move · / search · p people · s similar (search by image) ·
//       e description · t OCR text · o open in browser · q quit
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// ---------- entry ----------

func main() {
	cfg := loadConfig()
	if cfg.APIKey == "" {
		fmt.Fprintln(os.Stderr, "imtui: no api key — set api_key in ~/.config/imtui/config.toml (or IMTUI_KEY)")
		os.Exit(1)
	}
	c := NewClient(cfg.ServerURL, cfg.APIKey)
	p := tea.NewProgram(initialModel(c, cfg), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "imtui:", err)
		os.Exit(1)
	}
}

// ---------- config ----------

type Config struct {
	ServerURL string `toml:"server_url"`
	APIKey    string `toml:"api_key"`
	PreviewW  int    `toml:"preview_width"`
	PreviewH  int    `toml:"preview_height"`
}

const configPath = "~/.config/imtui/config.toml"

func loadConfig() Config {
	var c Config
	p := os.Getenv("IMTUI_CONFIG")
	if p == "" {
		p = expand(configPath)
	}
	b, err := os.ReadFile(p)
	if err == nil {
		toml.Unmarshal(b, &c)
	}
	if os.Getenv("IMTUI_URL") != "" {
		c.ServerURL = os.Getenv("IMTUI_URL")
	}
	if os.Getenv("IMTUI_KEY") != "" {
		c.APIKey = os.Getenv("IMTUI_KEY")
	}
	if c.ServerURL == "" {
		c.ServerURL = "http://localhost:2283"
	}
	if c.PreviewW <= 10 {
		c.PreviewW = 44
	}
	if c.PreviewH <= 5 {
		c.PreviewH = 20
	}
	return c
}

// ---------- styles ----------

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	subStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	selStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Bold(true)
	selMark     = lipgloss.NewStyle().Foreground(lipgloss.Color("213")).Render("▸")
	normalMark  = lipgloss.NewStyle().Foreground(lipgloss.Color("238")).Render("·")
	infoStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	borderStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("238"))
	footerKey   = lipgloss.NewStyle().Foreground(lipgloss.Color("213")).Bold(true)
	footerTxt   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
)

// ---------- messages ----------

type tickMsg struct{}

type resultsMsg struct {
	assets     []Asset
	total      int
	nextCursor string
	context    string // human description of what produced this list
	append     bool   // true = keep existing rows (paging), false = replace
	err        error
}

type detailMsg struct {
	asset Asset
	err   error
}

type previewMsg struct {
	id  string
	art string
	err error
}

type ocrMsg struct {
	id   string
	text string
	err  error
}

type savedMsg struct{ err error }

// ---------- model ----------

type view string // recent | search | people | person
type inputUse string

const (
	inputNone   inputUse = ""
	inputSearch inputUse = "search"
	inputDesc   inputUse = "description"
	inputRename inputUse = "rename"
)

type model struct {
	client  *Client
	cfg     Config
	view    view
	context string // header description of the current list ("recent", "similar to X", "people named Paul")

	// list state (search results, recent, or a person's photos)
	assets     []Asset
	nextCursor string // metadata-search cursor for incremental loading
	person     string // person id when view==person
	people     []PersonFull

	cursor  int
	preview string // chafa ANSI art of the selected asset
	pending string // asset id queued for preview render
	detail  map[string]Asset
	ocr     map[string]string
	ocrShow bool

	// input line
	input    textinput.Model
	inputUse inputUse

	status string
	width  int
	height int
	err    error
}

func initialModel(c *Client, cfg Config) model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.CharLimit = 300
	ti.Width = 60
	return model{
		client: c,
		cfg:    cfg,
		view:   "recent",
		detail: map[string]Asset{},
		ocr:    map[string]string{},
		input:  ti,
	}
}

func (m model) Init() tea.Cmd {
	return loadRecent(m.client)
}

// ---------- commands ----------

// loadRecent lists the newest photos (paged by cursor).
func loadRecent(c *Client) tea.Cmd {
	return func() tea.Msg {
		assets, next, err := c.MetadataSearch(nil, "", true)
		return resultsMsg{assets: assets, nextCursor: next, context: "recent", err: err}
	}
}

// loadPerson lists a person's photos (paged by cursor).
func loadPerson(c *Client, person string) tea.Cmd {
	return func() tea.Msg {
		assets, next, err := c.MetadataSearch([]string{person}, "", false)
		return resultsMsg{assets: assets, nextCursor: next, err: err}
	}
}

// runSearch wraps a smart search into a resultsMsg command.
func runSearch(c *Client, query, assetID string, ocr bool, context string) tea.Cmd {
	return func() tea.Msg {
		assets, total, err := c.SmartSearch(query, assetID, ocr, 100)
		return resultsMsg{assets: assets, total: total, context: context, err: err}
	}
}

// loadMore continues the current paged listing (recent or person).
func (m model) loadMore() tea.Cmd {
	var ids []string
	if m.person != "" && (m.view == "person") {
		ids = []string{m.person}
	}
	return func() tea.Msg {
		assets, next, err := m.client.MetadataSearch(ids, m.nextCursor, m.view == "recent")
		return resultsMsg{assets: assets, nextCursor: next, append: true, err: err}
	}
}

func fetchDetail(c *Client, id string) tea.Cmd {
	return func() tea.Msg {
		a, err := c.AssetDetail(id)
		return detailMsg{asset: a, err: err}
	}
}

func fetchOcr(c *Client, id string) tea.Cmd {
	return func() tea.Msg {
		t, err := c.OcrText(id)
		return ocrMsg{id: id, text: t, err: err}
	}
}

// renderPreview downloads the thumbnail (disk-cached) and runs chafa,
// restricted to ANSI symbols (half-blocks) — never the kitty protocol.
func renderPreview(c *Client, id string, cols, rows int) tea.Cmd {
	return func() tea.Msg {
		cache := filepath.Join(expand("~/.cache/imtui"), "thumbs", id+".jpg")
		if err := c.Thumbnail(id, cache); err != nil {
			return previewMsg{id: id, err: err}
		}
		out, err := exec.Command("chafa",
			"--format", "symbols",
			"--symbols", "half+space",
			"--colors", "256",
			"-s", fmt.Sprintf("%dx%d", cols, rows),
			"--animate", "off",
			cache).Output()
		if err != nil {
			return previewMsg{id: id, err: fmt.Errorf("chafa: %v", err)}
		}
		// strip chafa's cursor show/hide; bubbletea owns the cursor
		art := strings.NewReplacer("\x1b[?25l", "", "\x1b[?25h", "").Replace(string(out))
		return previewMsg{id: id, art: art}
	}
}

func saveDescription(c *Client, id, desc string) tea.Cmd {
	return func() tea.Msg {
		return savedMsg{c.UpdateDescription(id, desc)}
	}
}

func savePersonName(c *Client, id, name string) tea.Cmd {
	return func() tea.Msg {
		return savedMsg{c.RenamePerson(id, name)}
	}
}

// ---------- helpers ----------

func shortDate(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

func (m model) selected() *Asset {
	if m.cursor < 0 || m.cursor >= len(m.assets) {
		return nil
	}
	return &m.assets[m.cursor]
}

// ---------- update ----------

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height

	case tickMsg:
		// debounce: only render when the pending id is still the selection
		if a := m.selected(); a != nil && m.pending == a.ID {
			m.pending = ""
			return m, tea.Batch(
				renderPreview(m.client, a.ID, m.previewW(), m.previewH()),
				fetchDetail(m.client, a.ID),
			)
		}

	case resultsMsg:
		if msg.err != nil {
			m.err = msg.err
			m.status = "search failed"
			return m, nil
		}
		if msg.append {
			m.assets = append(m.assets, msg.assets...)
		} else {
			m.assets = msg.assets
			m.cursor = 0
			m.preview = ""
			m.ocrShow = false
			if msg.context != "" {
				m.context = msg.context
			}
		}
		m.nextCursor = msg.nextCursor
		m.pending = ""
		if msg.append {
			if len(msg.assets) == 0 {
				m.status = ""
			} else {
				m.status = fmt.Sprintf("%d results (paged)", len(m.assets))
			}
		} else {
			m.status = fmt.Sprintf("%d results", len(m.assets))
		}
		return m, m.schedulePreview()

	case detailMsg:
		if msg.err == nil {
			m.detail[msg.asset.ID] = msg.asset
		}

	case previewMsg:
		if msg.err != nil {
			m.status = "preview: " + msg.err.Error()
		}
		if a := m.selected(); a != nil && a.ID == msg.id {
			m.preview = msg.art
		}

	case ocrMsg:
		if msg.err != nil {
			m.status = "ocr: " + msg.err.Error()
			return m, nil
		}
		m.ocr[msg.id] = msg.text
		m.ocrShow = true
		m.status = ""

	case savedMsg:
		if msg.err != nil {
			m.status = "save failed: " + msg.err.Error()
		} else {
			m.status = "saved ✓"
			if a := m.selected(); a != nil {
				return m, fetchDetail(m.client, a.ID)
			}
		}

	case peopleMsg:
		if msg.err != nil {
			m.err = msg.err
			m.status = "cannot list people: " + msg.err.Error()
			return m, nil
		}
		m.people = msg.people
		m.cursor = 0
		if len(msg.people) == 0 {
			m.status = "no people detected yet"
		}

	case tea.KeyMsg:
		k := msg.String()

		// typing in the input line
		if m.inputUse != "" {
			switch k {
			case "esc":
				m.stopInput()
			case "enter":
				return m, m.submitInput()
			default:
				var cmd tea.Cmd
				m.input, cmd = m.input.Update(msg)
				return m, cmd
			}
			return m, nil
		}

		switch k {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "j", "down":
			m.cursor++
			return m, m.afterCursorMove()
		case "k", "up":
			m.cursor--
			return m, m.afterCursorMove()
		case "g", "home":
			m.cursor = 0
			return m, m.afterCursorMove()
		case "G", "end":
			m.cursor = 1 << 30
			return m, m.afterCursorMove()
		case "esc":
			switch m.view {
			case "person":
				m.view = "people"
				m.assets = nil
				m.preview = ""
				m.cursor = 0
				m.nextCursor = ""
				m.person = ""
				m.context = ""
				return m, m.loadPeople()
			case "search", "people":
				m.view = "recent"
				m.assets = nil
				m.preview = ""
				m.nextCursor = ""
				m.cursor = 0
				m.context = ""
				m.person = ""
				return m, loadRecent(m.client)
			}
		case "p":
			m.view = "people"
			m.assets = nil
			m.preview = ""
			m.cursor = 0
			m.nextCursor = ""
			m.context = ""
			return m, m.loadPeople()
		case "/":
			m.startInput(inputSearch, "")
		case "e":
			if a := m.selected(); a != nil {
				d := m.detail[a.ID]
				cur := ""
				if d.ExifInfo != nil {
					cur = d.ExifInfo.Description
				}
				m.startInput(inputDesc, cur)
			}
		case "r":
			if m.view == "people" && m.cursor >= 0 && m.cursor < len(m.people) {
				m.startInput(inputRename, m.people[m.cursor].Name)
			}
	case "enter":
			if m.view == "people" && m.cursor >= 0 && m.cursor < len(m.people) {
				p := m.people[m.cursor]
				m.view = "person"
				m.person = p.ID
				m.context = "people named " + p.Name
				m.assets = nil
				m.preview = ""
				m.nextCursor = ""
				m.cursor = 0
				return m, loadPerson(m.client, p.ID)
			}
		case "s":
			// search by image: find photos similar to the selected one
			if a := m.selected(); a != nil {
				return m, runSearch(m.client, "", a.ID, false,
					"similar to "+a.OriginalFileName)
			}
		case "t":
			if a := m.selected(); a != nil {
				if t, ok := m.ocr[a.ID]; ok {
					m.ocrShow = !m.ocrShow
					_ = t
				} else {
					m.status = "reading text…"
					return m, fetchOcr(m.client, a.ID)
				}
			}
		case "o":
			if a := m.selected(); a != nil {
				url := m.client.base + "/photos/" + a.ID
				if err := exec.Command("open", url).Start(); err != nil {
					m.status = "cannot open browser: " + err.Error()
				} else {
					m.status = "opened in browser"
				}
			}
		}
	}
	return m, nil
}

// schedulePreview queues a debounced preview render for the selection and
// returns the tick command that fires it (batched with a detail fetch).
func (m *model) schedulePreview() tea.Cmd {
	if a := m.selected(); a != nil {
		m.pending = a.ID
		return tea.Tick(300*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
	}
	m.pending = ""
	return nil
}

// afterCursorMove clamps the cursor, keeps the preview/debounce in sync, and
// loads the next page when the list nears its end.
func (m *model) afterCursorMove() tea.Cmd {
	m.clampCursor()
	var cmds []tea.Cmd
	if tick := m.schedulePreview(); tick != nil {
		cmds = append(cmds, tick)
	}
	if len(m.assets) > 0 && m.cursor >= len(m.assets)-10 && m.nextCursor != "" {
		cmds = append(cmds, m.loadMore())
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

func (m *model) clampCursor() {
	n := len(m.assets)
	if n == 0 {
		m.cursor = 0
		return
	}
	if m.cursor >= n {
		m.cursor = n - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// startInput focuses the input line for a purpose.
func (m *model) startInput(use inputUse, prefill string) {
	m.inputUse = use
	switch use {
	case inputSearch:
		m.input.Placeholder = "describe what you're looking for…"
		m.input.Prompt = "search: "
	case inputDesc:
		m.input.Placeholder = "what's in this photo?"
		m.input.Prompt = "description: "
	case inputRename:
		m.input.Placeholder = "person's name"
		m.input.Prompt = "name: "
	}
	m.input.SetValue(prefill)
	m.input.Focus()
	m.input.CursorEnd()
}

// stopInput unfocuses without submitting.
func (m *model) stopInput() {
	m.input.Blur()
	m.inputUse = inputNone
}

// submitInput dispatches the input line by purpose.
func (m *model) submitInput() tea.Cmd {
	val := strings.TrimSpace(m.input.Value())
	use := m.inputUse
	m.stopInput()
	switch use {
	case inputSearch:
		if val == "" {
			return nil
		}
		// "ocr <query>" restricts the search to OCR'd text
		if rest, ok := strings.CutPrefix(val, "ocr "); ok && rest != "" {
			return runSearch(m.client, rest, "", true, "OCR text “"+rest+"”")
		}
		return runSearch(m.client, val, "", false, "search “"+val+"”")
	case inputDesc:
		if a := m.selected(); a != nil {
			return saveDescription(m.client, a.ID, val)
		}
	case inputRename:
		if m.view == "people" && m.cursor >= 0 && m.cursor < len(m.people) {
			p := m.people[m.cursor]
			if val != "" && val != p.Name {
				return savePersonName(m.client, p.ID, val)
			}
		}
	}
	return nil
}

func (m model) loadPeople() tea.Cmd {
	return func() tea.Msg {
		ps, err := m.client.People()
		return peopleMsg{people: ps, err: err}
	}
}

type peopleMsg struct {
	people []PersonFull
	err    error
}

// ---------- sizing ----------

func (m model) previewW() int {
	w := m.cfg.PreviewW
	if m.width > 0 && w > m.width-30 {
		w = m.width - 30
	}
	if w < 20 {
		w = 20
	}
	return w
}

func (m model) previewH() int {
	h := m.cfg.PreviewH
	max := m.height - 7
	if max < 6 {
		max = 6
	}
	if h > max {
		h = max
	}
	return h
}

// ---------- view ----------

func (m model) View() string {
	if m.width == 0 {
		return "starting…"
	}
	var b strings.Builder
	b.WriteString(m.header() + "\n\n")

	var body string
	if m.view == "people" {
		b.WriteString(m.peopleList())
	} else {
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.listBlock(), "  ", m.paneBlock())
		b.WriteString(body)
	}
	b.WriteString("\n")
	b.WriteString(m.footer())
	return b.String()
}

func (m model) header() string {
	title := titleStyle.Render("imtui") + " " + subStyle.Render(m.client.base)
	ctx := ""
	if m.context != "" {
		ctx = subStyle.Render("  ·  " + m.context)
	}
	return title + ctx
}

// listW is the column width of the asset list (left pane).
const listW = 52

func (m model) listBlock() string {
	var rows []string
	if len(m.assets) == 0 {
		if m.pendingBuckets() {
			rows = append(rows, dimStyle.Render("loading…"))
		} else {
			rows = append(rows, dimStyle.Render("(no photos — press / to search, p for people)"))
		}
	} else {
		show := m.visibleCount()
		start := m.cursor - show/2
		if start < 0 {
			start = 0
		}
		end := start + show
		if end > len(m.assets) {
			end = len(m.assets)
			if start = end - show; start < 0 {
				start = 0
			}
		}
		for i := start; i < end; i++ {
			a := m.assets[i]
			mark, style := normalMark, infoStyle
			if i == m.cursor {
				mark, style = selMark, selStyle
			}
			line := fmt.Sprintf("%s %s %s",
				mark, dimStyle.Render(shortDate(a.CreatedAt)), style.Render(truncate(a.OriginalFileName, listW-14)))
			rows = append(rows, clipWidth(line, listW))
		}
	}
	return strings.Join(rows, "\n")
}

// paneBlock is the right-hand preview: chafa art + metadata underneath.
func (m model) paneBlock() string {
	a := m.selected()
	if a == nil {
		return ""
	}
	var art string
	if m.preview != "" {
		art = clipLines(m.preview, m.previewH())
	} else {
		art = dimStyle.Render(strings.Repeat("·\n", m.previewH()/2))
	}
	boxW := m.previewW() + 2
	box := borderStyle.Render(clipWidth(art, boxW))

	var meta []string
	if d, ok := m.detail[a.ID]; ok {
		if d.ExifInfo != nil {
			if d.ExifInfo.Description != "" {
				meta = append(meta, infoStyle.Render("“"+truncate(d.ExifInfo.Description, boxW)+"”"))
			}
			loc := []string{}
			for _, l := range []string{d.ExifInfo.City, d.ExifInfo.State, d.ExifInfo.Country} {
				if l != "" {
					loc = append(loc, l)
				}
			}
			if len(loc) > 0 {
				meta = append(meta, subStyle.Render(strings.Join(loc, ", ")))
			}
		}
		if names := personNames(d); len(names) > 0 {
			meta = append(meta, subStyle.Render("people: "+truncate(strings.Join(names, ", "), boxW)))
		}
	}
	if m.ocrShow {
		if t, ok := m.ocr[a.ID]; ok {
			if t == "" {
				t = "(no text detected)"
			}
			meta = append(meta, subStyle.Render("OCR: "+wordWrap(t, boxW)))
		}
	}
	return strings.Join(append([]string{box}, meta...), "\n")
}

// clipWidth hard-caps each line's visual width (ANSI-aware), so
// escape-laden art never blows out the layout.
func clipWidth(s string, w int) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, w, "")
	}
	return strings.Join(lines, "\n")
}

func wordWrap(s string, w int) string {
	words := strings.Fields(s)
	var lines []string
	cur := ""
	for _, wd := range words {
		if len(cur)+len(wd)+1 > w {
			lines = append(lines, cur)
			cur = wd
		} else if cur == "" {
			cur = wd
		} else {
			cur += " " + wd
		}
		if len(lines) >= 6 {
			break
		}
	}
	if cur != "" && len(lines) < 7 {
		lines = append(lines, cur)
	}
	return strings.Join(lines, "\n")
}

func (m model) peopleList() string {
	if len(m.people) == 0 {
		return dimStyle.Render("loading… (or no people detected yet)")
	}
	var rows []string
	show := m.visibleCount()
	start := m.cursor - show/2
	if start < 0 {
		start = 0
	}
	end := start + show
	if end > len(m.people) {
		end = len(m.people)
		if start = end - show; start < 0 {
			start = 0
		}
	}
	for i := start; i < end; i++ {
		p := m.people[i]
		mark, style := normalMark, infoStyle
		if i == m.cursor {
			mark, style = selMark, selStyle
		}
		name := p.Name
		if name == "" {
			name = "(unnamed)"
		}
		extra := ""
		if p.BirthDate != "" {
			extra = dimStyle.Render("  b. " + p.BirthDate)
		}
		rows = append(rows, fmt.Sprintf("%s %s%s", mark, style.Render(truncate(name, 30)), extra))
	}
	return strings.Join(rows, "\n")
}

func (m model) visibleCount() int {
	n := m.height - 12
	if n < 5 {
		n = 5
	}
	return n
}

func (m model) pendingBuckets() bool {
	return len(m.assets) == 0 && (m.view == "recent" || m.view == "person")
}
// (infoPane removed — paneBlock handles the preview column now.)

func personNames(a Asset) []string {
	out := []string{}
	for _, p := range a.People {
		if p.Name != "" {
			out = append(out, p.Name)
		}
	}
	return out
}

func clipLines(s string, maxLines int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	return strings.Join(lines, "\n")
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func (m model) footer() string {
	if m.inputUse != "" {
		return m.input.View()
	}
	hints := []struct{ k, v string }{
		{"j/k", "move"}, {"/", "search"}, {"p", "people"},
		{"s", "similar"}, {"e", "describe"}, {"t", "ocr"},
		{"o", "open"}, {"q", "quit"},
	}
	if m.view == "people" {
		hints = []struct{ k, v string }{
			{"j/k", "move"}, {"enter", "browse"}, {"r", "name"}, {"q", "quit"},
		}
	}
	parts := []string{}
	if m.status != "" {
		parts = append(parts, subStyle.Render(m.status))
	}
	if m.err != nil {
		parts = append(parts, footerKey.Render("err: "+m.err.Error()))
	}
	for _, h := range hints {
		parts = append(parts, footerKey.Render(h.k)+" "+footerTxt.Render(h.v))
	}
	return strings.Join(parts, " · ")
}
