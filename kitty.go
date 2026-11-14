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
	"os"
	"os/exec"
	"strings"
	"sync"

	_ "image/jpeg"
	_ "image/png"

	"golang.org/x/image/draw"
)

const (
	// Assumed cell pixel size when the terminal can't be queried. This is
	// kitty's common default font grid (and what chafa falls back to).
	kittyCellW = 8
	kittyCellH = 20
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
		if os.Getenv("IMTUI_PROTOCOL") == "kitty" {
			return "kitty"
		}
		return "symbols"
	}
	switch cfg.ImageProtocol {
	case "kitty":
		return "kitty"
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
	cache := expand(fmt.Sprintf("~/.cache/imtui/kitty/%d_%dx%d.png", fnv32a(path), cols, rows))
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
		// resize into the cell grid's pixel aspect so the terminal's stretch
		// to cols×rows cells doesn't distort the image
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
