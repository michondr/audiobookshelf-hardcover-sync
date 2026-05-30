package abs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// ── types ──────────────────────────────────────────────────────────────────

type librariesResponse struct {
	Libraries []Library `json:"libraries"`
}

type Library struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
}

type itemsResponse struct {
	Results []LibraryItem `json:"results"`
	Total   int           `json:"total"`
	Limit   int           `json:"limit"`
	Page    int           `json:"page"`
}

type LibraryItem struct {
	ID      string    `json:"id"`
	AddedAt int64     `json:"addedAt"` // milliseconds since epoch
	Media   mediaInfo `json:"media"`
}

type mediaInfo struct {
	Metadata bookMetadata `json:"metadata"`
	Duration float64      `json:"duration"` // total audiobook length in seconds
}

type bookMetadata struct {
	Title      string `json:"title"`
	AuthorName string `json:"authorName"`
	ISBN       string `json:"isbn"`
	ASIN       string `json:"asin"`
}

type MediaProgress struct {
	IsFinished  bool       `json:"isFinished"`
	Progress    float64    `json:"progress"`
	CurrentTime float64    `json:"currentTime"`
	LastUpdate  int64      `json:"lastUpdate"` // milliseconds
	StartedAt   *time.Time
	FinishedAt  *time.Time
}

type meResponse struct {
	MediaProgress []struct {
		LibraryItemID string   `json:"libraryItemId"`
		IsFinished    bool     `json:"isFinished"`
		CurrentTime   float64  `json:"currentTime"`
		LastUpdate    int64    `json:"lastUpdate"`
		StartedAt     *int64   `json:"startedAt"`  // milliseconds, nullable
		FinishedAt    *int64   `json:"finishedAt"` // milliseconds, nullable
	} `json:"mediaProgress"`
}

// Book is the merged view of a library item + its progress.
type Book struct {
	ItemID       string
	Title        string
	Author       string
	ISBN         string
	ASIN         string
	AddedAt        time.Time
	TotalSeconds   float64
	CurrentSeconds float64
	IsFinished     bool
	LastUpdate     time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
}

// ── API calls ──────────────────────────────────────────────────────────────

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s: status %d: %s", path, resp.StatusCode, body)
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) GetLibraries(ctx context.Context) ([]Library, error) {
	var r librariesResponse
	if err := c.get(ctx, "/api/libraries", &r); err != nil {
		return nil, err
	}
	var out []Library
	for _, l := range r.Libraries {
		if l.MediaType == "book" {
			out = append(out, l)
		}
	}
	return out, nil
}

func (c *Client) GetLibraryItems(ctx context.Context, libraryID string) ([]LibraryItem, error) {
	var all []LibraryItem
	limit := 500
	page := 0

	for {
		var r itemsResponse
		path := fmt.Sprintf("/api/libraries/%s/items?limit=%d&page=%d", libraryID, limit, page)
		if err := c.get(ctx, path, &r); err != nil {
			return nil, err
		}
		all = append(all, r.Results...)
		if len(r.Results) < limit {
			break
		}
		page++
	}
	return all, nil
}

// getAllProgress returns a map of libraryItemID → MediaProgress for every item
// the user has interacted with, including finished books.
func (c *Client) getAllProgress(ctx context.Context) (map[string]*MediaProgress, error) {
	var r meResponse
	if err := c.get(ctx, "/api/me", &r); err != nil {
		return nil, err
	}
	m := make(map[string]*MediaProgress, len(r.MediaProgress))
	for _, p := range r.MediaProgress {
		mp := &MediaProgress{
			IsFinished:  p.IsFinished,
			CurrentTime: p.CurrentTime,
			LastUpdate:  p.LastUpdate,
		}
		if p.StartedAt != nil {
			t := time.Unix(*p.StartedAt/1000, 0)
			mp.StartedAt = &t
		}
		if p.FinishedAt != nil {
			t := time.Unix(*p.FinishedAt/1000, 0)
			mp.FinishedAt = &t
		}
		m[p.LibraryItemID] = mp
	}
	return m, nil
}

// GetAllBooks fetches all book-library items and merges in progress data.
// Results are sorted alphabetically by title for deterministic ordering.
func (c *Client) GetAllBooks(ctx context.Context) ([]Book, error) {
	libs, err := c.GetLibraries(ctx)
	if err != nil {
		return nil, fmt.Errorf("libraries: %w", err)
	}

	itemMap := map[string]LibraryItem{}
	for _, lib := range libs {
		items, err := c.GetLibraryItems(ctx, lib.ID)
		if err != nil {
			return nil, fmt.Errorf("library %s items: %w", lib.ID, err)
		}
		for _, item := range items {
			itemMap[item.ID] = item
		}
	}

	progressMap, err := c.getAllProgress(ctx)
	if err != nil {
		return nil, fmt.Errorf("user progress: %w", err)
	}

	books := make([]Book, 0, len(itemMap))
	for id, item := range itemMap {
		b := Book{
			ItemID:       id,
			Title:        item.Media.Metadata.Title,
			Author:       item.Media.Metadata.AuthorName,
			ISBN:         item.Media.Metadata.ISBN,
			ASIN:         item.Media.Metadata.ASIN,
			TotalSeconds: item.Media.Duration,
			AddedAt:      time.Unix(item.AddedAt/1000, 0),
		}
		if p, ok := progressMap[id]; ok {
			b.CurrentSeconds = p.CurrentTime
			b.IsFinished = p.IsFinished
			b.LastUpdate = time.Unix(p.LastUpdate/1000, 0)
			b.StartedAt = p.StartedAt
			b.FinishedAt = p.FinishedAt
		}
		books = append(books, b)
	}

	// Newest added in ABS first; fall back to title for ties.
	sort.Slice(books, func(i, j int) bool {
		if books[i].AddedAt.Equal(books[j].AddedAt) {
			return books[i].Title < books[j].Title
		}
		return books[i].AddedAt.After(books[j].AddedAt)
	})
	return books, nil
}

// BaseURL returns the configured ABS server base URL.
func (c *Client) BaseURL() string { return c.baseURL }

// ProxyCover fetches the cover image from ABS and returns raw bytes + content type.
// Returns an error if ABS responds with a non-image content type.
func (c *Client) ProxyCover(ctx context.Context, itemID string) ([]byte, string, error) {
	url := c.baseURL + "/api/items/" + itemID + "/cover"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("cover: status %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "image/") {
		return nil, "", fmt.Errorf("cover: unexpected content-type %q", ct)
	}

	const maxCoverSize = 8 << 20 // 8 MB
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCoverSize))
	return body, ct, err
}
