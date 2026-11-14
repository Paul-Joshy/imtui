package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ---------- client ----------

type Client struct {
	base      string
	key       string
	ownerID   string // filled by FetchOwner; used for direct thumbnail paths
	thumbsDir string // Immich data thumbs dir (server-local fast path)
	http      *http.Client
}

func NewClient(base, key string) *Client {
	return &Client{
		base: strings.TrimRight(base, "/"),
		key:  key,
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

// SetThumbsDir points the client at the server's generated thumbnail files
// (e.g. the docker volume mount .../library/thumbs). When set, previews are
// read straight from disk instead of the API.
func (c *Client) SetThumbsDir(dir string) {
	if dir != "" {
		c.thumbsDir = expand(dir)
	}
}

// FetchOwner caches the current user id (needed to locate thumbnail files).
func (c *Client) FetchOwner() error {
	b, err := c.do("GET", "/users/me", nil)
	if err != nil {
		return err
	}
	var u struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &u); err != nil {
		return err
	}
	c.ownerID = u.ID
	return nil
}

// ThumbPath returns a readable image file for the asset. preferPreview
// selects the large preview (kitty mode needs pixels); otherwise the small
// thumbnail. Served from the server's files when available, else the API
// (cached to cacheDir after first download).
func (c *Client) ThumbPath(id string, preferPreview bool, cacheDir string) (string, error) {
	if c.thumbsDir != "" && c.ownerID != "" && len(id) >= 4 {
		kind := "thumbnail.webp"
		if preferPreview {
			kind = "preview.jpeg"
		}
		p := filepath.Join(c.thumbsDir, c.ownerID, id[:2], id[2:4], id+"_"+kind)
		if fileExists(p) {
			return p, nil
		}
	}
	size, suffix := "thumbnail", "-small.jpg"
	if preferPreview {
		size, suffix = "preview", ".jpg"
	}
	cache := filepath.Join(cacheDir, "thumbs", id+suffix)
	if err := c.Thumbnail(id, cache, size); err != nil {
		return "", err
	}
	return cache, nil
}

func (c *Client) do(method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, c.base+"/api"+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: %s", method, path, strings.TrimSpace(string(b)))
	}
	return b, nil
}

// ---------- types ----------

type Asset struct {
	ID               string    `json:"id"`
	Type             string    `json:"type"`
	OriginalFileName string    `json:"originalFileName"`
	CreatedAt        string    `json:"createdAt"`
	ExifInfo         *ExifInfo `json:"exifInfo"`
	People           []Person  `json:"people"`
}

type ExifInfo struct {
	Description      string `json:"description"`
	DateTimeOriginal string `json:"dateTimeOriginal"`
	City             string `json:"city"`
	State            string `json:"state"`
	Country          string `json:"country"`
}

type Person struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type PersonFull struct {
	Person
	BirthDate string `json:"birthDate"`
}

type searchResponse struct {
	Assets struct {
		Total int     `json:"total"`
		Items []Asset `json:"items"`
		// the cursor field's name and type depend on the query style:
		// filter-style searches return nextCursor (base64 offset, string);
		// the deprecated top-level style returns nextPage (page number) —
		// as a JSON number, hence RawMessage
		NextCursor string          `json:"nextCursor"`
		NextPage   json.RawMessage `json:"nextPage"`
	} `json:"assets"`
}

// cursor returns the paging cursor for the response as a string, from
// whichever field the server populated.
func (sr searchResponse) cursor() string {
	if sr.Assets.NextCursor != "" {
		return sr.Assets.NextCursor
	}
	p := strings.TrimSpace(string(sr.Assets.NextPage))
	p = strings.Trim(p, `"`)
	if p == "" || p == "null" {
		return ""
	}
	return p
}

// ---------- calls ----------

// SmartSearch runs Immich's CLIP smart search: a text query searches photo
// context semantically; queryAssetId searches BY an image; ocr includes the
// OCR'd text layer. Returns assets + total.
func (c *Client) SmartSearch(query, queryAssetID string, ocr bool, size int) ([]Asset, int, error) {
	req := map[string]any{"size": size}
	if query != "" {
		req["query"] = query
	}
	if queryAssetID != "" {
		req["queryAssetId"] = queryAssetID
	}
	if ocr {
		req["ocr"] = "true" // server accepts it as a string
	}
	b, err := c.do("POST", "/search/smart", req)
	if err != nil {
		return nil, 0, err
	}
	var sr searchResponse
	if err := json.Unmarshal(b, &sr); err != nil {
		return nil, 0, err
	}
	return sr.Assets.Items, sr.Assets.Total, nil
}

// AssetDetail returns the full asset (people, exif, description).
func (c *Client) AssetDetail(id string) (Asset, error) {
	b, err := c.do("GET", "/assets/"+id, nil)
	if err != nil {
		return Asset{}, err
	}
	var a Asset
	err = json.Unmarshal(b, &a)
	return a, err
}

// UpdateDescription writes the asset description.
func (c *Client) UpdateDescription(id, desc string) error {
	_, err := c.do("PUT", "/assets/"+id, map[string]string{"description": desc})
	return err
}

// OcrText fetches the OCR text boxes of an asset and joins them with " · "
// (for display).
func (c *Client) OcrText(id string) (string, error) {
	t, err := c.ocrJoined(id, " · ")
	return t, err
}

// ocrJoined fetches the OCR text boxes of an asset and joins their text.
func (c *Client) ocrJoined(id, sep string) (string, error) {
	b, err := c.do("GET", "/assets/"+id+"/ocr", nil)
	if err != nil {
		return "", err
	}
	var boxes []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &boxes); err != nil {
		return "", err
	}
	parts := make([]string, 0, len(boxes))
	for _, bx := range boxes {
		if t := strings.TrimSpace(bx.Text); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, sep), nil
}

// OcrGrep greps the OCR'd text across the library: candidates come from the
// server's metadata ocr filter (a fast case-insensitive match), then each
// candidate's actual OCR text is fetched and checked, so the result is an
// exact substring match on the real text — grep semantics, not the fuzzy
// ML search. maxCandidates caps how many candidates get verified.
func (c *Client) OcrGrep(term string, maxCandidates int) ([]Asset, error) {
	b, err := c.do("POST", "/search/metadata", map[string]any{"ocr": term, "size": 1000})
	if err != nil {
		return nil, err
	}
	var sr searchResponse
	if err := json.Unmarshal(b, &sr); err != nil {
		return nil, err
	}
	cands := sr.Assets.Items
	if len(cands) > maxCandidates {
		cands = cands[:maxCandidates]
	}
	needle := strings.ToLower(term)
	// verify candidates concurrently — each OCR fetch is a separate request
	type hit struct {
		i int
		a Asset
	}
	hits := make(chan hit, len(cands))
	sem := make(chan struct{}, 8)
	for i, a := range cands {
		go func(i int, a Asset) {
			sem <- struct{}{}
			defer func() { <-sem }()
			t, err := c.ocrJoined(a.ID, " ")
			if err == nil && strings.Contains(strings.ToLower(t), needle) {
				hits <- hit{i, a}
			} else {
				hits <- hit{i, Asset{}}
			}
		}(i, a)
	}
	out := make([]Asset, len(cands))
	n := 0
	for range cands {
		h := <-hits
		if h.a.ID != "" {
			out[h.i] = h.a
			n++
		}
	}
	res := make([]Asset, 0, n)
	for _, a := range out {
		if a.ID != "" {
			res = append(res, a)
		}
	}
	return res, nil
}

func (c *Client) People() ([]PersonFull, error) {
	b, err := c.do("GET", "/people?withHidden=false", nil)
	if err != nil {
		return nil, err
	}
	var r struct {
		People []PersonFull `json:"people"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return r.People, nil
}

// SearchPeople finds people by (fuzzy) name match via Immich's search
// endpoint. The result set can be empty (no matches), which is not an error.
func (c *Client) SearchPeople(name string) ([]PersonFull, error) {
	b, err := c.do("GET", "/search/person?name="+url.QueryEscape(name)+"&withHidden=false", nil)
	if err != nil {
		return nil, err
	}
	var people []PersonFull
	if err := json.Unmarshal(b, &people); err != nil {
		return nil, err
	}
	return people, nil
}

// RenamePerson sets a person's name (Immich then re-labels their faces).
func (c *Client) RenamePerson(id, name string) error {
	_, err := c.do("PUT", "/people/"+id, map[string]string{"name": name})
	return err
}

// MetadataSearch lists assets by metadata: recent photos (no filters, newest
// first), a person's photos (personIDs), or by filename. Returns the assets
// plus the next cursor for paging ("" when done).
func (c *Client) MetadataSearch(personIDs []string, cursor string, newestFirst bool) ([]Asset, string, error) {
	req := map[string]any{"size": 100}
	if len(personIDs) > 0 {
		// The deprecated top-level personIds pages by page NUMBER: a cursor
		// alongside it is rejected, and the v3 filter style silently drops
		// the person filter once a cursor is present (server-side). So
		// person browse sends page numbers; the cursor carries "2", "3", …
		req["personIds"] = personIDs
		if cursor != "" {
			if n, err := strconv.Atoi(cursor); err == nil && n > 1 {
				req["page"] = n
			}
		}
	} else {
		if cursor != "" {
			req["cursor"] = cursor
		}
		if newestFirst {
			req["orderBy"] = map[string]string{"field": "fileCreatedAt", "direction": "desc"}
		}
	}
	b, err := c.do("POST", "/search/metadata", req)
	if err != nil {
		return nil, "", err
	}
	var sr searchResponse
	if err := json.Unmarshal(b, &sr); err != nil {
		return nil, "", err
	}
	return sr.Assets.Items, sr.cursor(), nil
}

// Original downloads the asset's original file (full resolution, original
// format) to dest, streaming so large videos don't buffer in memory.
func (c *Client) Original(id, dest string) error {
	req, err := http.NewRequest("GET", c.base+"/api/assets/"+id+"/original", nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("GET original: %s", resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// Thumbnail downloads a preview thumbnail to path (no-op if cached).
// size is "thumbnail" (small, fast) or "preview" (larger, for kitty images).
func (c *Client) Thumbnail(id, path, size string) error {
	if fileExists(path) {
		return nil
	}
	b, err := c.do("GET", "/assets/"+id+"/thumbnail?size="+size+"&format=JPEG", nil)
	if err != nil {
		return err
	}
	return writeFile(path, b)
}
