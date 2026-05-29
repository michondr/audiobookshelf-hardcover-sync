package abs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// --- types ---

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
	ID    string    `json:"id"`
	Media mediaInfo `json:"media"`
}

type mediaInfo struct {
	Metadata bookMetadata `json:"metadata"`
}

type bookMetadata struct {
	Title      string `json:"title"`
	AuthorName string `json:"authorName"`
	ISBN       string `json:"isbn"`
	ASIN       string `json:"asin"`
}

type inProgressResponse struct {
	LibraryItems []inProgressItem `json:"libraryItems"`
}

type inProgressItem struct {
	ID            string          `json:"id"`
	MediaProgress *MediaProgress  `json:"mediaProgress"`
	Media         mediaInfo       `json:"media"`
}

type MediaProgress struct {
	IsFinished  bool    `json:"isFinished"`
	Progress    float64 `json:"progress"`
	CurrentTime float64 `json:"currentTime"`
	LastUpdate  int64   `json:"lastUpdate"`
}

// Book is the merged view of a library item + its progress.
type Book struct {
	ItemID     string
	Title      string
	Author     string
	ISBN       string
	ASIN       string
	CurrentSeconds float64
	IsFinished     bool
	LastUpdate     time.Time
}

// --- API calls ---

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
	// only book libraries
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

func (c *Client) GetItemsInProgress(ctx context.Context) ([]inProgressItem, error) {
	var r inProgressResponse
	if err := c.get(ctx, "/api/me/items-in-progress", &r); err != nil {
		return nil, err
	}
	return r.LibraryItems, nil
}

// GetAllBooks fetches all book-library items and merges in progress data.
func (c *Client) GetAllBooks(ctx context.Context) ([]Book, error) {
	libs, err := c.GetLibraries(ctx)
	if err != nil {
		return nil, fmt.Errorf("libraries: %w", err)
	}

	// Collect all library items
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

	// Collect progress
	inProgress, err := c.GetItemsInProgress(ctx)
	if err != nil {
		return nil, fmt.Errorf("items-in-progress: %w", err)
	}
	progressMap := map[string]*MediaProgress{}
	for _, ip := range inProgress {
		if ip.MediaProgress != nil {
			progressMap[ip.ID] = ip.MediaProgress
		}
	}

	// Merge
	books := make([]Book, 0, len(itemMap))
	for id, item := range itemMap {
		b := Book{
			ItemID: id,
			Title:  item.Media.Metadata.Title,
			Author: item.Media.Metadata.AuthorName,
			ISBN:   item.Media.Metadata.ISBN,
			ASIN:   item.Media.Metadata.ASIN,
		}
		if p, ok := progressMap[id]; ok {
			b.CurrentSeconds = p.CurrentTime
			b.IsFinished = p.IsFinished
			b.LastUpdate = time.Unix(p.LastUpdate/1000, 0)
		}
		books = append(books, b)
	}
	return books, nil
}

// BaseURL returns the configured ABS server base URL.
func (c *Client) BaseURL() string { return c.baseURL }

// CoverURL returns the URL for proxying an ABS cover image.
// The actual proxying is done by the web server.
func (c *Client) CoverURL(itemID string) string {
	return c.baseURL + "/api/items/" + itemID + "/cover"
}

// ProxyCover fetches the cover image from ABS and returns the raw bytes + content type.
func (c *Client) ProxyCover(ctx context.Context, itemID string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.CoverURL(itemID), nil)
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

	body, err := io.ReadAll(resp.Body)
	return body, resp.Header.Get("Content-Type"), err
}
