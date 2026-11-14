package main

// iTerm2 inline images: OSC 1337. iTerm2 renders JPEG natively, so the
// Immich preview file is sent as-is — no PNG re-encode like kitty needs.
// The placement size (in cells) comes from the same aspect fit used for
// kitty mode; preserveAspectRatio=1 keeps iTerm2 from stretching if the
// assumed cell size is slightly off, and doNotMoveCursor=1 is iTerm2's
// equivalent of kitty's C=1 (the text renderer keeps owning the cursor).

import (
	"encoding/base64"
	"fmt"
	"os"
)

// itermImageSequence reads the image at path and returns the OSC 1337
// sequence that draws it over cols×rows cells at the cursor. iTerm2 accepts
// PNG, JPEG, GIF and more; Immich's previews are JPEG.
func itermImageSequence(path string, cols, rows int) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"\x1b]1337;File=inline=1;size=%d;width=%d;height=%d;preserveAspectRatio=1;doNotMoveCursor=1:%s\x07",
		len(b), cols, rows, base64.StdEncoding.EncodeToString(b),
	), nil
}
