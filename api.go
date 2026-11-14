package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
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
		Total      int     `json:"total"`
		Items      []Asset `json:"items"`
		NextCursor string  `json:"nextCursor"`
	} `json:"assets"`
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

// OcrText fetches the OCR text boxes of an asset and joins them.
func (c *Client) OcrText(id string) (string, error) {
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
	return strings.Join(parts, " · "), nil
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
		req["personIds"] = personIDs
	}
	if cursor != "" {
		req["cursor"] = cursor
	}
	if newestFirst {
		req["orderBy"] = map[string]string{"field": "fileCreatedAt", "direction": "desc"}
	}
	b, err := c.do("POST", "/search/metadata", req)
	if err != nil {
		return nil, "", err
	}
	var sr searchResponse
	if err := json.Unmarshal(b, &sr); err != nil {
		return nil, "", err
	}
	return sr.Assets.Items, sr.Assets.NextCursor, nil
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
