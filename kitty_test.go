package main

import (
	"bytes"
	"encoding/base64"
	"image/png"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// findCachedThumb picks a real JPEG from the disk cache to test with.
func findCachedThumb(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(expand("~/.cache/imtui/thumbs"))
	if err != nil {
		t.Skip("no thumbnail cache")
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jpg") && !strings.Contains(e.Name(), "-small") {
			return expand("~/.cache/imtui/thumbs/" + e.Name())
		}
	}
	t.Skip("no cached thumbnail")
	return ""
}

func TestKittySequenceStructure(t *testing.T) {
	path := findCachedThumb(t)
	start := time.Now()
	transmit, place, err := kittyImageSequence(path, 44, 20, 7)
	if err != nil {
		t.Fatal(err)
	}
	encode := time.Since(start)
	t.Logf("transmit: %d bytes (%d chunks), place: %d bytes, encode %v",
		len(transmit), strings.Count(transmit, "\x1b_G"), len(place), encode)

	if strings.Count(transmit, "\x1b_G") != strings.Count(transmit, "\x1b\\") {
		t.Fatal("unbalanced APC blocks")
	}
	if !strings.HasPrefix(transmit, "\x1b_Ga=T,f=100,t=d,q=2,i=7,p=1,c=44,r=20,C=1,m=") {
		t.Fatalf("bad first params: %q", transmit[:52])
	}
	if strings.Contains(transmit, "C=0") || !strings.Contains(transmit, "C=1") {
		t.Fatal("transmit must pin the no-cursor-movement policy (C=1)")
	}
	want := "\x1b_Ga=p,i=7,p=1,q=2,c=44,r=20,C=1\x1b\\"
	if place != want {
		t.Fatalf("bad place seq: %q", place)
	}
}

// fitCells must preserve the photo's proportions: the chosen cell rect's
// pixel size (cols*cw × rows*ch) matches the image aspect as exactly as the
// cell grid allows, and never exceeds the budget.
func TestFitCells(t *testing.T) {
	kittyCellW, kittyCellH = 8, 16 // 1:2 cells like the default assumption
	cases := []struct {
		name       string
		A          float64 // source width/height
		bw, bh     int
		wantErrMax float64 // max relative aspect error tolerated
	}{
		{"square", 1.0, 44, 20, 0.005},
		{"portrait 2:3", 2.0 / 3.0, 44, 20, 0.005},
		{"portrait 3:4", 3.0 / 4.0, 44, 20, 0.005},
		{"landscape 4:3", 4.0 / 3.0, 44, 20, 0.005},
		{"landscape 16:9", 16.0 / 9.0, 44, 20, 0.005},
		{"panorama", 3.0, 44, 20, 0.005},
		{"narrow portrait 9:16", 9.0 / 16.0, 44, 20, 0.005},
	}
	for _, tc := range cases {
		c, r := fitCells(tc.A, tc.bw, tc.bh)
		if c < 1 || r < 1 || c > tc.bw || r > tc.bh {
			t.Fatalf("%s: out of budget %dx%d: %dx%d", tc.name, tc.bw, tc.bh, c, r)
		}
		got := float64(c) * 8 / (float64(r) * 16)
		err := math.Abs(got-tc.A) / tc.A
		if err > tc.wantErrMax {
			t.Errorf("%s: %dx%d cells → aspect %.4f, want %.4f (err %.1f%%)",
				tc.name, c, r, got, tc.A, err*100)
		}
	}
}

// images are resized 1:1 onto the placement cells: the bitmap dims must
// equal cols*cellW × rows*cellH (checked via the sequence's PNG header).
func TestKittyBitmapMatchesPlacement(t *testing.T) {
	path := findCachedThumb(t)
	kittyCellW, kittyCellH = 8, 16
	cols, rows := 30, 15
	transmit, _, err := kittyImageSequence(path, cols, rows, 5)
	if err != nil {
		t.Fatal(err)
	}
	// extract the base64 payload of the single-chunk transmit
	i := strings.Index(transmit, ";")
	j := strings.Index(transmit[i:], "\x1b\\")
	raw, err := base64.StdEncoding.DecodeString(transmit[i+1 : i+j])
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != cols*8 || cfg.Height != rows*16 {
		t.Fatalf("bitmap %dx%d, want %dx%d", cfg.Width, cfg.Height, cols*8, rows*16)
	}
}

func TestParseCellSize(t *testing.T) {
	// kitty: direct cell size report
	if w, h := parseCellSize("\x1b[?25l\x1b[6;20;9t"); w != 9 || h != 20 {
		t.Fatalf("16t: got %dx%d, want 9x20", w, h)
	}
	// window pixels + window cells
	s := "\x1b[4;400;72t\x1b[8;20;9t\x1b[0m"
	if w, h := parseCellSize(s); w != 8 || h != 20 {
		t.Fatalf("14t+18t: got %dx%d, want 8x20", w, h)
	}
	if w, h := parseCellSize("garbage"); w != 0 || h != 0 {
		t.Fatalf("garbage: got %dx%d", w, h)
	}
}

func TestKittyDelete(t *testing.T) {
	want := "\x1b_Ga=d,d=i,i=42,q=2\x1b\\\\"
	if got := kittyDelete(42); got != want {
		t.Fatalf("delete seq:\n got %q\nwant %q", got, want)
	}
}

func TestSettleFreshPreview(t *testing.T) {
	id := "11111111-2222-3333-4444-555555555555"
	prev := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	m := model{
		proto:   "kitty",
		assets:  []Asset{{ID: id}},
		cursor:  0,
		preview: "",
		previewCache: map[string]cachedArt{
			id + ":44x20": {art: "TRANSMIT+PLACE", place: "PLACE", fresh: true},
		},
		placeCache: map[string]string{},
		cfg:        Config{PreviewW: 44, PreviewH: 20},
	}
	m.width, m.height = 120, 34

	// display the previous image, then the new one — showArt appends the
	// delete of the previous image and tracks the ids
	m.lastKittyID = 0 // first display: nothing to delete
	m.showArt(prev, "OLD", 0, 0)
	if strings.Contains(m.preview, "a=d") {
		t.Fatalf("first display must not delete: %q", m.preview)
	}
	m.showArt(id, "TRANSMIT+PLACE", 44, 20)
	if !strings.HasSuffix(m.preview, kittyDelete(kittyIDFor(prev))) {
		t.Fatalf("second display must delete the previous image: %q", m.preview)
	}
	if m.lastKittyID != kittyIDFor(id) || m.freshID != id {
		t.Fatalf("state not tracked: lastKittyID=%d freshID=%q", m.lastKittyID, m.freshID)
	}

	// too soon: the transmit may not have been flushed yet
	c := m.previewCache[m.previewKey(id)]
	c.shownAt = time.Now()
	m.previewCache[m.previewKey(id)] = c
	m.settleFreshPreview()
	if !strings.HasPrefix(m.preview, "TRANSMIT+PLACE") {
		t.Fatalf("settled too early: preview = %q", m.preview)
	}

	// after the guard window: swap to the cheap place sequence
	c.shownAt = time.Now().Add(-100 * time.Millisecond)
	m.previewCache[m.previewKey(id)] = c
	m.settleFreshPreview()
	if m.preview != "PLACE" {
		t.Fatalf("settled preview:\n got %q\nwant %q", m.preview, "PLACE")
	}
	if m.freshID != "" {
		t.Fatalf("freshID not cleared: %q", m.freshID)
	}
	if c := m.previewCache[m.previewKey(id)]; c.fresh || c.art != "PLACE" {
		t.Fatalf("cache not downgraded: %+v", c)
	}
	if m.placeCache[m.previewKey(id)] != "PLACE" {
		t.Fatal("placeCache missing after settle")
	}
}

// showArt must chain deletes: displaying B after A removes A in the same
// frame; re-displaying A re-places it without deleting B's id prematurely.
func TestShowArtDeletesPrevious(t *testing.T) {
	a, b, c := "aaaaaaaa", "bbbbbbbb", "cccccccc"
	m := model{proto: "kitty"}
	m.showArt(a, "A", 44, 20)
	if strings.Contains(m.preview, "a=d") {
		t.Fatalf("first display must not delete: %q", m.preview)
	}
	m.showArt(b, "B", 44, 20)
	if !strings.HasSuffix(m.preview, kittyDelete(kittyIDFor(a))) {
		t.Fatalf("second display must delete the first: %q", m.preview)
	}
	m.showArt(c, "C", 44, 20)
	if !strings.HasSuffix(m.preview, kittyDelete(kittyIDFor(b))) {
		t.Fatalf("third display must delete the second: %q", m.preview)
	}
	m.showArt(c, "C", 44, 20)
	if strings.Contains(m.preview, "a=d") {
		t.Fatalf("re-displaying the current image must not delete: %q", m.preview)
	}
}

func TestKittyErasePadding(t *testing.T) {
	m := model{
		proto:   "kitty",
		assets:  []Asset{{ID: "x"}},
		cursor:  0,
		preview: "\x1b_Ga=T…\x1b\\\\",
		cfg:     Config{PreviewW: 44, PreviewH: 20},
	}
	m.width, m.height = 120, 34
	rows := strings.Split(m.assetViewKitty(), "\n")
	for i := 0; i < 20; i++ {
		if !strings.HasSuffix(rows[i], "\x1b[44C") {
			t.Fatalf("row %d lacks cursor-forward padding: %q", i, rows[i])
		}
	}
	// metadata rows below the image area must stay clean
	if i := 20; i < len(rows) && strings.HasSuffix(rows[i], "\x1b[44C") {
		t.Fatalf("metadata row %d should not be padded", i)
	}
}

func TestTmuxPassthrough(t *testing.T) {
	t.Setenv("TMUX", "/tmp/fake")
	place := "\x1b_Ga=p,i=7,p=0,q=2,c=44,r=20\x1b\\"
	got := wrapTmuxAPC(place)
	want := "\x1bPtmux;Ga=p,i=7,p=0,q=2,c=44,r=20\x1b\\"
	if got != want {
		t.Fatalf("wrapped:\n got %q\nwant %q", got, want)
	}
	// payload with an embedded ESC must be doubled
	seq := "\x1b_Ga=d,q=2\x1b\\"
	got = wrapTmuxAPC(seq)
	want = "\x1bPtmux;Ga=d,q=2\x1b\\"
	if got != want {
		t.Fatalf("simple wrap:\n got %q\nwant %q", got, want)
	}
	// non-APC text must pass through untouched
	plain := "\x1b[31mhello\x1b[0m"
	if got := wrapTmuxAPC(plain); got != plain {
		t.Fatalf("plain text mangled: %q", got)
	}
}
