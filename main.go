// imtui — a minimal Immich client for the terminal.
//
// Browse recent photos, search by context (CLIP), by OCR'd text, or BY an
// image; browse and rename people; edit photo descriptions. Previews render
// through chafa.
//
// Keys: j/k move · / search · p people · s similar (search by image) ·
//
//	e description · t OCR text · o open in browser · q quit
package main

import (
	"fmt"
	"os"
	"os/exec"
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
	c.SetThumbsDir(cfg.ThumbsDir)
	// best-effort: only needed for direct disk reads of thumbnails
	_ = c.FetchOwner()

	m := initialModel(c, cfg)
	m.proto = detectImageProtocol(cfg)
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "imtui:", err)
		os.Exit(1)
	}
}

// ---------- config ----------

type Config struct {
	ServerURL     string `toml:"server_url"`
	APIKey        string `toml:"api_key"`
	PreviewW      int    `toml:"preview_width"`
	PreviewH      int    `toml:"preview_height"`
	ImageProtocol string `toml:"image_protocol"` // auto | kitty | symbols
	ThumbsDir     string `toml:"thumbs_dir"`     // Immich library/thumbs for direct disk reads
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
	search     bool   // true when produced by a smart search (not a listing)
	append     bool   // true = keep existing rows (paging), false = replace
	err        error
}

type detailMsg struct {
	asset Asset
	err   error
}

type previewMsg struct {
	id    string
	art   string
	place string // kitty: cheap re-place sequence for later frames
	err   error
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
	inputName   inputUse = "namesearch"
	inputDesc   inputUse = "description"
	inputRename inputUse = "rename"
)

// cachedArt is a rendered preview: art is what's drawn first (symbols ANSI
// or a kitty transmit+place); place is the cheap re-display sequence for
// later frames (symbols mode leaves it empty — art is reused as-is).
// fresh means art still carries the full transmit that has not been
// swapped for place yet; shownAt is when it became the visible preview.
type cachedArt struct {
	art     string
	place   string
	fresh   bool
	shownAt time.Time
}

type model struct {
	client  *Client
	cfg     Config
	proto   string // image protocol: "symbols" or "kitty"
	view    view
	context string // header description of the current list ("recent", "similar to X", "people named Paul")

	// list state (search results, recent, or a person's photos)
	assets      []Asset
	nextCursor  string // metadata-search cursor for incremental loading
	loadingMore bool   // a page fetch is in flight (dedupes repeat requests)
	person      string // person id when view==person
	people      []PersonFull

	cursor       int
	preview      string // art of the selected asset (ANSI or kitty sequences)
	pending      string // asset id queued for preview render
	previewCache map[string]cachedArt
	placeCache   map[string]string // "id:WxH" -> cheap re-place sequence (after first draw)
	freshID      string            // asset whose preview still contains the transmit
	lastKittyID  int               // kitty image id of the image currently on screen
	lastShow     time.Time         // when the last kitty art was displayed (burst throttle)
	detail       map[string]Asset
	ocr          map[string]string
	ocrShow      bool

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
		client:       c,
		cfg:          cfg,
		view:         "recent",
		detail:       map[string]Asset{},
		ocr:          map[string]string{},
		previewCache: map[string]cachedArt{},
		placeCache:   map[string]string{},
		input:        ti,
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
		return resultsMsg{assets: assets, total: total, context: context, search: true, err: err}
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

// renderPreview prepares the preview art for one asset.
//   - symbols mode: chafa half-blocks over a small thumbnail (~9 KB)
//   - kitty mode: the terminal draws real pixels — we resize the full
//     preview image and speak the kitty graphics protocol (see kitty.go)
func renderPreview(c *Client, id string, cols, rows int, proto string) tea.Cmd {
	return func() tea.Msg {
		path, err := c.ThumbPath(id, proto == "kitty", expand("~/.cache/imtui"))
		if err != nil {
			return previewMsg{id: id, err: err}
		}
		if proto == "kitty" {
			transmit, place, err := kittyImageSequence(path, cols, rows, kittyIDFor(id))
			if err != nil {
				return previewMsg{id: id, err: err}
			}
			// art = transmit + place for the first draw; later frames
			// re-place with the cheap sequence only.
			return previewMsg{
				id:    id,
				art:   wrapTmuxAPC(transmit + place),
				place: wrapTmuxAPC(place),
			}
		}
		out, err := exec.Command("chafa",
			"--format", "symbols",
			"--symbols", "half+space",
			"--colors", "256",
			"-s", fmt.Sprintf("%dx%d", cols, rows),
			"--animate", "off",
			path).Output()
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
	if os.Getenv("IMTUI_DEBUG") != "" {
		if _, isKey := msg.(tea.KeyMsg); !isKey {
			fmt.Fprintf(os.Stderr, "[msg %T]\n", msg)
		}
	}
	m.settleFreshPreview()
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height

	case tickMsg:
		// debounce fired: render the preview (only if still the selection)
		if a := m.selected(); a != nil && m.pending == a.ID {
			m.pending = ""
			cmds := []tea.Cmd{renderPreview(m.client, a.ID, m.previewW(), m.previewH(), m.proto)}
			if _, ok := m.detail[a.ID]; !ok {
				cmds = append(cmds, fetchDetail(m.client, a.ID))
			}
			return m, tea.Batch(cmds...)
		}

	case resultsMsg:
		if msg.err != nil {
			m.err = msg.err
			m.status = "search failed"
			return m, nil
		}
		if msg.append {
			m.assets = append(m.assets, msg.assets...)
			m.loadingMore = false
		} else {
			m.assets = msg.assets
			m.cursor = 0
			m.preview = ""
			m.freshID = ""
			m.loadingMore = false
			m.ocrShow = false
			if msg.context != "" {
				m.context = msg.context
			}
			if msg.search {
				m.view = "search"
				m.person = ""
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
		if msg.art != "" {
			m.previewCache[m.previewKey(msg.id)] = cachedArt{
				art:     msg.art,
				place:   msg.place,
				fresh:   m.proto == "kitty",
				shownAt: time.Now(),
			}
		}
		if a := m.selected(); a != nil && a.ID == msg.id && msg.art != "" {
			m.showArt(msg.id, msg.art)
			// pre-render the rows above/below so scrolling hits the cache
			return m, m.preRenderNeighbors()
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
		m.context = msg.context // empty = full list (resets any name filter)
		m.status = ""
		if len(msg.people) == 0 {
			if msg.context != "" {
				m.status = "no matching people"
			} else {
				m.status = "no people detected yet"
			}
		}

	case tea.KeyMsg:
		// bubbletea groups a burst of printable keys (e.g. held "jjjjj" or
		// fast typing) into ONE KeyMsg — split it so every keypress acts.
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 {
			var cmds []tea.Cmd
			mm := m
			for _, r := range msg.Runes {
				var cmd tea.Cmd
				mm, cmd = mm.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
				if cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
			return mm, tea.Batch(cmds...)
		}
		return m.handleKey(msg)
	}
	return m, nil
}

// handleKey processes a single key press. Bursts of printable keys are
// split by Update and routed here one rune at a time.
func (m model) handleKey(msg tea.KeyMsg) (model, tea.Cmd) {
	k := msg.String()
	if os.Getenv("IMTUI_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[key %q cursor=%d assets=%d]\n", k, m.cursor, len(m.assets))
	}

	// typing in the input line
	if m.inputUse != "" {
		switch k {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			m.stopInput()
		case "enter":
			return m, m.submitInput()
		case "q":
			// an empty input line is never what you wanted — q dismisses it
			if m.input.Value() == "" {
				m.stopInput()
				return m, nil
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
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
		// contextual: search people by name from the people view,
		// otherwise run the photo (CLIP/OCR) search
		if m.view == "people" {
			m.startInput(inputName, "")
		} else {
			m.startInput(inputSearch, "")
		}
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
	return m, nil
}

// showArt makes art the visible preview for the asset. Any previously
// displayed image is deleted in the same frame, after the new art — so
// during fast scrolling exactly one image stays on screen and the new one
// is already on top (monotonic ids) before the old goes away: no flash.
// Lowercase d=i removes placements but keeps the image data, so scrolling
// back to a shown asset can still re-place it from placeCache without a
// re-transmit.
func (m *model) showArt(id, art string) {
	kid := kittyIDFor(id)
	if m.lastKittyID != 0 && m.lastKittyID != kid {
		art += kittyDelete(m.lastKittyID)
	}
	m.preview = art
	m.lastKittyID = kid
	m.freshID = id
	m.lastShow = time.Now()
}

// settleFreshPreview runs at the start of every Update: when the selected
// asset's preview still carries the full kitty transmit and has been on
// screen long enough to have been flushed to the terminal, it swaps the view
// to the cheap re-place sequence. The swap changes the rendered row, so the
// next redraw executes it — ~30 bytes instead of re-sending ~230KB on
// every subsequent frame.
func (m *model) settleFreshPreview() {
	if m.proto != "kitty" || m.freshID == "" {
		return
	}
	a := m.selected()
	if a == nil || a.ID != m.freshID {
		return
	}
	key := m.previewKey(a.ID)
	c, ok := m.previewCache[key]
	if !ok || !c.fresh || c.place == "" {
		return
	}
	// bubbletea flushes at ~60fps; if the next message arrives sooner than
	// this, the transmit may not have reached the terminal yet — keep
	// waiting rather than risk replacing it before it ever drew.
	if time.Since(c.shownAt) < 50*time.Millisecond {
		return
	}
	m.preview = c.place
	c.art = c.place // cache now serves the cheap sequence too
	c.fresh = false
	m.previewCache[key] = c
	m.placeCache[key] = c.place
	m.freshID = ""
}

// schedulePreview shows the preview for the selection: instantly from cache
// when possible, otherwise schedules a debounced render. Returns the command
// to run (detail fetch and/or tick), or nil.
func (m *model) schedulePreview() tea.Cmd {
	a := m.selected()
	if a == nil {
		m.pending = ""
		return nil
	}
	key := m.previewKey(a.ID)
	if place, ok := m.placeCache[key]; ok {
		// drawn before: the image is already uploaded to the terminal
		m.showArt(a.ID, place)
		m.pending = ""
		if _, has := m.detail[a.ID]; !has {
			return fetchDetail(m.client, a.ID)
		}
		return nil
	}
	if c, ok := m.previewCache[key]; ok {
		if c.fresh {
			// still carrying the transmit (pre-rendered neighbor): it will
			// be swapped to place by settleFreshPreview once displayed.
			c.shownAt = time.Now()
			m.previewCache[key] = c
		}
		if time.Since(m.lastShow) < 120*time.Millisecond {
			// scrolling fast: don't stream ~230KB per step through tmux —
			// collapse the burst into one transmit when the keys pause.
			m.pending = a.ID
			return tea.Tick(60*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
		}
		m.showArt(a.ID, c.art)
		m.pending = ""
		return fetchDetailIfMissing(m, a.ID)
	}
	m.pending = a.ID
	return tea.Tick(60*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

func fetchDetailIfMissing(m *model, id string) tea.Cmd {
	if _, has := m.detail[id]; has {
		return nil
	}
	return fetchDetail(m.client, id)
}

// previewKey identifies a cached render: asset id + pane dimensions.
func (m *model) previewKey(id string) string {
	return fmt.Sprintf("%s:%dx%d", id, m.previewW(), m.previewH())
}

// preRenderNeighbors renders the rows just above/below the cursor into the
// cache, so single-step scrolling shows previews without waiting.
func (m model) preRenderNeighbors() tea.Cmd {
	var cmds []tea.Cmd
	for _, i := range []int{m.cursor - 1, m.cursor + 1} {
		if i < 0 || i >= len(m.assets) {
			continue
		}
		id := m.assets[i].ID
		key := m.previewKey(id)
		if _, ok := m.previewCache[key]; ok {
			continue
		}
		cmds = append(cmds, renderPreview(m.client, id, m.previewW(), m.previewH(), m.proto))
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// afterCursorMove clamps the cursor, keeps the preview/debounce in sync, and
// loads the next page when the list nears its end.
func (m *model) afterCursorMove() tea.Cmd {
	m.clampCursor()
	if m.view == "people" {
		return nil // people list: no preview, no paging
	}
	var cmds []tea.Cmd
	if os.Getenv("IMTUI_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[page cursor=%d len=%d next=%q loading=%v]\n",
			m.cursor, len(m.assets), m.nextCursor, m.loadingMore)
	}
	if tick := m.schedulePreview(); tick != nil {
		cmds = append(cmds, tick)
	}
	if len(m.assets) > 0 && m.cursor >= len(m.assets)-10 && m.nextCursor != "" && !m.loadingMore {
		m.loadingMore = true
		cmds = append(cmds, m.loadMore())
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

func (m *model) clampCursor() {
	// clamp against whatever list the cursor indexes right now
	n := len(m.assets)
	if m.view == "people" {
		n = len(m.people)
	}
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
	case inputName:
		m.input.Placeholder = "who are you looking for…"
		m.input.Prompt = "names: "
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
	case inputName:
		if val == "" {
			// empty: back to the full people list
			return m.loadPeople()
		}
		return runPeopleSearch(m.client, val)
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

// runPeopleSearch narrows the people list to those whose name matches.
func runPeopleSearch(c *Client, name string) tea.Cmd {
	return func() tea.Msg {
		ps, err := c.SearchPeople(name)
		return peopleMsg{
			people:  ps,
			context: "people matching “" + name + "”",
			err:     err,
		}
	}
}

type peopleMsg struct {
	people  []PersonFull
	context string
	err     error
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
		body = m.assetView()
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

// assetView composes the list (left) and preview pane (right).
func (m model) assetView() string {
	if m.proto == "kitty" {
		return m.assetViewKitty()
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, m.listBlock(), "  ", m.paneBlock())
}

// assetViewKitty joins list and pane line-by-line. The image line carries
// only the list text and then the raw graphics sequence — anything drawn
// after it on the same row (padding spaces) would land on the image, so it
// is deliberately the last thing on its line.
func (m model) assetViewKitty() string {
	left := strings.Split(m.listBlock(), "\n")
	right := m.paneLinesKitty()
	n := len(left)
	if len(right) > n {
		n = len(right)
	}
	rows := make([]string, 0, n)
	for i := 0; i < n; i++ {
		l := ""
		if i < len(left) {
			l = left[i]
		}
		l = padRight(l, listW)
		r := ""
		if i < len(right) {
			r = right[i]
		}
		row := l + "  " + r
		if i < m.previewH() {
			// The image occupies previewH terminal rows starting at this
			// pane's column. bubbletea appends ESC[K (erase to end of line)
			// to every changed row narrower than the terminal, which would
			// erase straight across the image cells on each scroll step.
			// Appending a cursor-forward parks the cursor past the image's
			// right edge, so the erase lands on empty space instead. CUF
			// prints nothing, so it is safe even on the graphics line.
			row += fmt.Sprintf("\x1b[%dC", m.previewW())
		}
		rows = append(rows, row)
	}
	return strings.Join(rows, "\n")
}

func padRight(s string, w int) string {
	pad := w - lipgloss.Width(s)
	if pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
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
	return strings.Join(append([]string{box}, m.assetMeta(boxW)...), "\n")
}

// paneLinesKitty is the right-hand pane for kitty graphics mode: the image
// occupies previewH rows (the graphics sequence is the first line's entire
// content — nothing may be printed after it), then metadata below.
func (m model) paneLinesKitty() []string {
	a := m.selected()
	if a == nil {
		return nil
	}
	var lines []string
	if m.preview != "" {
		lines = append(lines, m.preview)
	} else {
		lines = append(lines, dimStyle.Render(strings.Repeat("·", m.previewW())))
	}
	for i := 1; i < m.previewH(); i++ {
		lines = append(lines, "")
	}
	return append(lines, m.assetMeta(64)...)
}

// assetMeta renders the description / location / people / OCR block.
func (m model) assetMeta(w int) []string {
	a := m.selected()
	if a == nil {
		return nil
	}
	var meta []string
	if d, ok := m.detail[a.ID]; ok {
		if d.ExifInfo != nil {
			if d.ExifInfo.Description != "" {
				meta = append(meta, infoStyle.Render("“"+truncate(d.ExifInfo.Description, w)+"”"))
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
			meta = append(meta, subStyle.Render("people: "+truncate(strings.Join(names, ", "), w)))
		}
	}
	if m.ocrShow {
		if t, ok := m.ocr[a.ID]; ok {
			if t == "" {
				t = "(no text detected)"
			}
			meta = append(meta, subStyle.Render("OCR: "+wordWrap(t, w)))
		}
	}
	return meta
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
	// The people view has no image rows, but a kitty image from the asset
	// view may still be placed on screen — and graphics draw *above* text.
	// Emit an idempotent delete on the first line so it can't cover the
	// list (re-deleting a gone image is a no-op; data is kept so scrolling
	// back to assets can re-place it from the cache).
	if m.proto == "kitty" && m.lastKittyID != 0 {
		return kittyDelete(m.lastKittyID) + "\n" + m.peopleRows()
	}
	return m.peopleRows()
}

func (m model) peopleRows() string {
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
			{"j/k", "move"}, {"/", "names"}, {"enter", "browse"}, {"r", "name"}, {"q", "quit"},
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
