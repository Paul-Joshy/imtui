// kitty.go — real-pixel previews via the kitty graphics protocol.
//
// The kitty protocol lets a terminal display actual images. We resize the
// thumbnail to the pane's cell grid, PNG-encode it (kitty accepts PNG
// payloads directly, f=100 — far smaller than raw RGBA), and send
// transmit+display in one placement. Later redraws of the same image use a
// tiny "place" command that references the already-uploaded image.
//
// Inside tmux the sequences are wrapped in DCS passthrough (tmux forwards
// them to the outer terminal untouched); this needs `allow-passthrough on`.
package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"image"
	"image/png"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	_ "image/jpeg"
	_ "image/png"

	"golang.org/x/image/draw"
)

// Assumed cell pixel size (1:2, matching most terminal fonts and chafa's
// assumption) until queryCellSize refines it to the real terminal grid.
var (
	kittyCellW = 8
	kittyCellH = 16
)

// kitty image ids must increase monotonically as new photos are displayed:
// overlapping placements with the same z-index stack by id (higher id on
// top), so a hash/random id would sometimes draw the NEW photo underneath
// the old one until cleanup ran — an old→new swap the eye reads as tearing.
// Ids are allocated per asset and reused for re-transmits/re-places.
var (
	kittyMu   sync.Mutex
	kittyIDs  = map[string]int{}
	kittyNext int
)

// kittyIDFor returns a stable, monotonically increasing kitty image id for
// the asset id.
func kittyIDFor(assetID string) int {
	kittyMu.Lock()
	defer kittyMu.Unlock()
	if id, ok := kittyIDs[assetID]; ok {
		return id
	}
	kittyNext++
	kittyIDs[assetID] = kittyNext
	return kittyNext
}

// fnv32a hashes s for cache file naming (stable across sessions).
func fnv32a(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

// kittyDelete returns the sequence that deletes the image with the given id
// (removes all its placements and frees its data). Used to clean up the
// previously displayed preview once a new one is on screen.
func kittyDelete(id int) string {
	return fmt.Sprintf("\x1b_Ga=d,d=i,i=%d,q=2\x1b\\\\", id)
}

// detectImageProtocol resolves cfg.ImageProtocol: explicit "kitty"/"symbols"
// wins; "auto" (or empty) checks kitty's env markers, TERM, and — inside
// tmux — the terminal type of the attached client.
func detectImageProtocol(cfg Config) string {
	switch {
	case os.Getenv("IMTUI_PROTOCOL") != "":
		switch os.Getenv("IMTUI_PROTOCOL") {
		case "kitty":
			return "kitty"
		case "iterm":
			return "iterm"
		}
		return "symbols"
	}
	switch cfg.ImageProtocol {
	case "kitty":
		return "kitty"
	case "iterm":
		return "iterm"
	case "symbols":
		return "symbols"
	}
	if os.Getenv("KITTY_WINDOW_ID") != "" {
		return "kitty"
	}
	if strings.Contains(strings.ToLower(os.Getenv("TERM")), "kitty") {
		return "kitty"
	}
	if os.Getenv("TERM_PROGRAM") == "kitty" {
		return "kitty"
	}
	// iTerm2 speaks its own OSC 1337 inline-image protocol
	if os.Getenv("TERM_PROGRAM") == "iTerm.app" || os.Getenv("ITERM_SESSION_ID") != "" {
		return "iterm"
	}
	if os.Getenv("TMUX") != "" {
		if out, err := exec.Command("tmux", "display-message", "-p", "#{client_termname}").Output(); err == nil {
			if strings.Contains(strings.ToLower(string(out)), "kitty") {
				return "kitty"
			}
		}
	}
	return "symbols"
}

// kittyImageSequence reads the image at path and returns two sequences:
// transmit (upload + display, for the first draw) and place (re-display the
// uploaded image on later frames). The image is drawn over cols×rows cells
// starting at the terminal cursor. Encoded PNGs are cached on disk so each
// asset is resized/encoded once per session's pane size.
//
// The sequence parameters matter for rendering stability inside tmux:
//   - C=1: kitty must not move the cursor after display (default C=0 moves
//     it right/down by the image size, which tmux does not track through
//     passthrough — subsequent output lands rows away = screen tearing).
//   - p=1: a fixed placement id, so re-placing replaces the placement
//     instead of stacking a new one (p=0 auto-assigns a fresh id each time).
//   - q=2: no responses back from the terminal.
func kittyImageSequence(path string, cols, rows, id int) (transmit, place string, err error) {
	var pngBytes []byte
	cache := expand(fmt.Sprintf("~/.cache/imtui/kitty/%d_%dx%d_%dx%d.png",
		fnv32a(path), cols, rows, kittyCellW, kittyCellH))
	if b, err := os.ReadFile(cache); err == nil {
		pngBytes = b
	} else {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", "", err
		}
		src, _, err := image.Decode(bytes.NewReader(b))
		if err != nil {
			return "", "", fmt.Errorf("decode thumbnail: %w", err)
		}
		// resize to exactly the placement's pixel grid so the terminal's
		// cell mapping is 1:1 and proportions are preserved
		dst := image.NewRGBA(image.Rect(0, 0, cols*kittyCellW, rows*kittyCellH))
		draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)

		var buf bytes.Buffer
		enc := png.Encoder{CompressionLevel: png.BestSpeed}
		if err := enc.Encode(&buf, dst); err != nil {
			return "", "", err
		}
		pngBytes = buf.Bytes()
		_ = os.MkdirAll(expand("~/.cache/imtui/kitty"), 0o755)
		_ = os.WriteFile(cache, pngBytes, 0o644)
	}
	b64 := base64.StdEncoding.EncodeToString(pngBytes)

	// a=T transmit+display, t=d direct payload, f=100 PNG, q=2 no response
	// frames, i/p ids for later re-placement, c/r the cell area. Payloads
	// are chunked at 4096 base64 chars (m=1, final chunk m=0).
	const chunk = 4096
	var sb strings.Builder
	for sent, total := 0, len(b64); sent < total; {
		end := sent + chunk
		if end > total {
			end = total
		}
		first, last := sent == 0, end == total
		var params string
		switch {
		case first && last:
			params = fmt.Sprintf("a=T,f=100,t=d,q=2,i=%d,p=1,c=%d,r=%d,C=1,m=0", id, cols, rows)
		case first:
			params = fmt.Sprintf("a=T,f=100,t=d,q=2,i=%d,p=1,c=%d,r=%d,C=1,m=1", id, cols, rows)
		case last:
			params = "m=0"
		default:
			params = "m=1"
		}
		sb.WriteString("\x1b_G" + params + ";" + b64[sent:end] + "\x1b\\")
		sent = end
	}
	transmit = sb.String()
	place = fmt.Sprintf("\x1b_Ga=p,i=%d,p=1,q=2,c=%d,r=%d,C=1\x1b\\", id, cols, rows)
	return transmit, place, nil
}

// fitCells returns the cell rectangle (cols×rows, within the budget) whose
// on-screen pixel size best matches the image aspect A — i.e. the largest
// area with the least proportion error, searched over every row count. The
// resized bitmap then maps 1:1 onto the placement cells, so photo
// proportions are preserved exactly.
func fitCells(A float64, bw, bh int) (cols, rows int) {
	bestErr := math.Inf(1)
	bestArea := 0
	for r := bh; r >= 1; r-- {
		c := int(math.Round(A * float64(r) * float64(kittyCellH) / float64(kittyCellW)))
		if c < 1 {
			c = 1
		}
		if c > bw {
			continue // too wide for this r (and wider r only gets worse)
		}
		err := math.Abs(float64(c)*float64(kittyCellW)/(float64(r)*float64(kittyCellH))-A) / A
		area := c * r
		if err < bestErr-1e-9 || (err < bestErr+1e-9 && area > bestArea) {
			bestErr, bestArea, cols, rows = err, area, c, r
		}
	}
	if cols == 0 {
		cols, rows = 1, 1
	}
	return cols, rows
}

// queryCellSize asks the terminal for the actual pixel size of a character
// cell so placement preserves photo proportions exactly. Sources, in order:
// tmux's own knowledge (#{client_cell_width/height}, when it has it), then
// direct CSI queries (16 t cell size; 14 t window pixels + 18 t window
// cells). Best-effort — on any failure the 1:2 default is kept.
func queryCellSize() {
	cw, ch := 0, 0
	if os.Getenv("TMUX") != "" {
		if out, err := exec.Command("tmux", "display-message", "-p",
			"#{client_cell_width} #{client_cell_height}").Output(); err == nil {
			fmt.Sscanf(string(out), "%d %d", &cw, &ch)
		}
	}
	if cw <= 0 || ch <= 0 {
		cw, ch = csiQueryCellSize()
	}
	if cw > 0 && ch > 0 && cw <= 100 && ch <= 200 {
		kittyCellW, kittyCellH = cw, ch
		if os.Getenv("IMTUI_DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "[cell %dx%d]\n", cw, ch)
		}
	}
}

// csiQueryCellSize sends the CSI queries and parses the responses.
func csiQueryCellSize() (w, h int) {
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return 0, 0
	}
	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return 0, 0
	}
	defer term.Restore(int(os.Stdin.Fd()), state)
	if err := os.Stdin.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		return 0, 0
	}
	q := wrapTmuxPassthroughCSI("\x1b[16t\x1b[14t\x1b[18t")
	os.Stdout.WriteString(q)

	buf := make([]byte, 256)
	n, _ := os.Stdin.Read(buf)
	return parseCellSize(string(buf[:n]))
}

// parseCellSize extracts the cell pixel size from terminal query replies.
// Recognizes CSI 6;<h>;<w>t (direct cell size) and the window-pixels (4;…)
// + window-cells (8;…) pair used to derive it.
func parseCellSize(s string) (w, h int) {
	if i := strings.Index(s, "\x1b[6;"); i >= 0 {
		if _, err := fmt.Sscanf(s[i:], "\x1b[6;%d;%dt", &h, &w); err == nil && w > 0 && h > 0 {
			return w, h
		}
	}
	i := strings.Index(s, "\x1b[4;")
	if i < 0 {
		return 0, 0
	}
	var winH, winW, rows, cols int
	if _, err := fmt.Sscanf(s[i:], "\x1b[4;%d;%dt", &winH, &winW); err != nil {
		return 0, 0
	}
	j := strings.Index(s, "\x1b[8;")
	if j < 0 {
		return 0, 0
	}
	if _, err := fmt.Sscanf(s[j:], "\x1b[8;%d;%dt", &rows, &cols); err != nil || cols <= 0 || rows <= 0 {
		return 0, 0
	}
	return winW / cols, winH / rows
}

// wrapTmuxPassthroughCSI DCS-wraps plain CSI sequences for tmux passthrough
// (tmux requires ESC bytes inside the payload to be doubled).
func wrapTmuxPassthroughCSI(s string) string {
	if os.Getenv("TMUX") == "" {
		return s
	}
	return "\x1bPtmux;" + strings.ReplaceAll(s, "\x1b", "\x1b\x1b") + "\x1b\\"
}

// wrapTmuxAPC wraps every APC block (ESC _ … ESC \) in tmux DCS passthrough
// so the sequence reaches the outer terminal instead of tmux's screen.
// tmux requires ESC bytes inside the payload to be doubled.
func wrapTmuxAPC(s string) string {
	if os.Getenv("TMUX") == "" {
		return s
	}
	var out strings.Builder
	for i := 0; i < len(s); {
		rel := strings.Index(s[i:], "\x1b_")
		if rel < 0 {
			out.WriteString(s[i:])
			break
		}
		start := i + rel
		relEnd := strings.Index(s[start+2:], "\x1b\\")
		if relEnd < 0 {
			out.WriteString(s[i:])
			break
		}
		end := start + 2 + relEnd + 2
		payload := strings.ReplaceAll(s[start+2:end-2], "\x1b", "\x1b\x1b")
		out.WriteString(s[i:start])
		out.WriteString("\x1bPtmux;")
		out.WriteString(payload)
		out.WriteString("\x1b\\")
		i = end
	}
	return out.String()
}
