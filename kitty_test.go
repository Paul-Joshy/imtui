package main

import (
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
	if !strings.HasPrefix(transmit, "\x1b_Ga=T,f=100,t=d,q=2,i=7,p=0,c=44,r=20,m=") {
		t.Fatalf("bad first params: %q", transmit[:48])
	}
	want := "\x1b_Ga=p,i=7,p=0,q=2,c=44,r=20\x1b\\"
	if place != want {
		t.Fatalf("bad place seq: %q", place)
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
		freshID: id,
		preview: "TRANSMIT+PLACE",
		previewCache: map[string]cachedArt{
			id + ":44x20": {art: "TRANSMIT+PLACE", place: "PLACE", fresh: true},
		},
		placeCache:  map[string]string{},
		lastKittyID: kittyID(prev),
		cfg:         Config{PreviewW: 44, PreviewH: 20},
	}
	m.width, m.height = 120, 34 // previewKey depends on the pane size

	// too soon: the transmit may not have been flushed yet
	c := m.previewCache[m.previewKey(id)]
	c.shownAt = time.Now()
	m.previewCache[m.previewKey(id)] = c
	m.settleFreshPreview()
	if m.preview != "TRANSMIT+PLACE" {
		t.Fatalf("settled too early: preview = %q", m.preview)
	}

	// after the guard window: swap to place + delete the previous image
	c.shownAt = time.Now().Add(-100 * time.Millisecond)
	m.previewCache[m.previewKey(id)] = c
	m.settleFreshPreview()
	want := "PLACE" + kittyDelete(kittyID(prev))
	if m.preview != want {
		t.Fatalf("settled preview:\n got %q\nwant %q", m.preview, want)
	}
	if m.freshID != "" || m.lastKittyID != kittyID(id) {
		t.Fatalf("state not updated: freshID=%q lastKittyID=%d", m.freshID, m.lastKittyID)
	}
	if c := m.previewCache[m.previewKey(id)]; c.fresh || c.art != "PLACE" {
		t.Fatalf("cache not downgraded: %+v", c)
	}
	if m.placeCache[m.previewKey(id)] != "PLACE" {
		t.Fatal("placeCache missing after settle")
	}

	// re-settling the same asset must not delete itself
	m.settleFreshPreview()
	if m.lastKittyID != kittyID(id) {
		t.Fatalf("lastKittyID changed on re-settle: %d", m.lastKittyID)
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
