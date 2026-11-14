# imtui

A minimal [Immich](https://immich.app) client for the terminal. Browse your
library with photo previews rendered right in the pane (via
[chafa](https://hpjansson.com/chafa/)), search your photos by meaning, by
text in them, or by *another photo*, and tag things without opening a
browser.

```
▸ 2026-11-08 IMG-20261108-WA0033.jpg    ╭──────────────────────────╮
· 2026-11-08 IMG-20261108-WA0032.jpg    │        ▄▄▄▄▄▄▄▄▄▄▄▄      │
· 2026-11-08 Screenshot_2026-11-…       │   ▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄    │
                                        ╰──────────────────────────╯
                                        "WhatsApp photo from Nov 2026"
                                        people: Ekta
```

## Features

- **Browse** — newest photos first, paged as you scroll
- **Real-pixel previews** — in kitty/ghostty-class terminals, photos are
  drawn with the kitty graphics protocol (actual pixels, not ASCII art);
  falls back to crisp chafa half-block previews everywhere else
- **Fast** — thumbnails are read straight off the server's disk when the
  library is local, renders are cached in memory and on disk, and neighbors
  are pre-rendered as you move
- **Search by context** — CLIP smart search: type what you're looking for
  ("dog on the beach", "screenshot", "birthday cake")
- **Search by image** — press `s` on any photo to find visually similar ones
- **Search OCR'd text** — Immich reads text inside photos; `ocr receipt`
  finds photos containing that text. Press `t` to read the text of the
  selected photo
- **People** — browse everyone Immich has detected, view their photos, and
  rename people (press `r`)
- **Descriptions** — press `e` to write/edit a photo's description
- **Open in browser** — `o` opens the asset in the Immich web app

## Keys

| Key | Action |
| --- | --- |
| `j` / `k` | move (vim-style), `g`/`G` top/bottom |
| `/` | search (prefix with `ocr ` to search text inside photos) |
| `p` | people list · `enter` browse · `r` rename person |
| `s` | find photos similar to the selected one |
| `e` | edit description |
| `t` | show OCR'd text |
| `o` | open in browser |
| `esc` | back |
| `q` | quit (`ctrl+c` works anywhere, even mid-input) |

## Install

```sh
go build -o ~/.local/bin/imtui .
```

Requires: an Immich server (v3+ recommended). For symbol previews also
install [chafa](https://hpjansson.com/chafa/) (`brew install chafa`); the
kitty graphics path needs no external tools. `o` uses macOS `open`.

## Configuration

`~/.config/imtui/config.toml` (created by hand; env overrides `IMTUI_URL`,
`IMTUI_KEY`, `IMTUI_CONFIG`, `IMTUI_PROTOCOL`):

```toml
server_url = "http://localhost:2283"
api_key    = "your-immich-api-key"   # Immich web → Account → API Keys
preview_width  = 44
preview_height = 20
# generated thumbnails on disk — read directly instead of the API when the
# Immich library is local (docker compose usually mounts ./library)
thumbs_dir = "~/Documents/immich-app/library/thumbs"
# preview rendering: "auto" (detect kitty), "kitty", or "symbols" (ANSI art)
image_protocol = "auto"
```

Get an API key from the Immich web UI: **Account Settings → API Keys →
New API Key**. The key stays in that local file.

## Notes

- Thumbnails and search are read-mostly; the only writes are description
  edits and person renames, both explicit keypresses.
- Previews are cached in `~/.cache/imtui/` (`thumbs/` from the API,
  `kitty/` as encoded images for the graphics protocol).
- **Kitty graphics + tmux**: tmux must forward the sequences — put
  `set -g allow-passthrough on` in `~/.tmux.conf` (and reload it) if images
  don't appear inside tmux.

## License

MIT
