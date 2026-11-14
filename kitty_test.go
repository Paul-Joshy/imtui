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
	m.showArt(prev, "OLD")
	if strings.Contains(m.preview, "a=d") {
		t.Fatalf("first display must not delete: %q", m.preview)
	}
	m.showArt(id, "TRANSMIT+PLACE")
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
	m.showArt(a, "A")
	if strings.Contains(m.preview, "a=d") {
		t.Fatalf("first display must not delete: %q", m.preview)
	}
	m.showArt(b, "B")
	if !strings.HasSuffix(m.preview, kittyDelete(kittyIDFor(a))) {
		t.Fatalf("second display must delete the first: %q", m.preview)
	}
	m.showArt(c, "C")
	if !strings.HasSuffix(m.preview, kittyDelete(kittyIDFor(b))) {
		t.Fatalf("third display must delete the second: %q", m.preview)
	}
	m.showArt(c, "C")
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
