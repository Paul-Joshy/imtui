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
