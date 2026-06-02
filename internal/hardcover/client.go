package hardcover

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const apiURL = "https://api.hardcover.app/v1/graphql"

const (
	StatusWantToRead       = 1
	StatusCurrentlyReading = 2
	StatusRead             = 3
	StatusDidNotFinish     = 5
	FormatAudiobook        = 2
	FormatEbook            = 4
)

// minRequestInterval spaces out requests to stay under Hardcover's rate limit
// (~60/min). Without this, a bulk match pass gets throttled and requests come
// back with empty bodies.
const minRequestInterval = 1100 * time.Millisecond

type Client struct {
	token string
	http  *http.Client

	mu      sync.Mutex // guards lastReq; serializes the rate-limit gate
	lastReq time.Time
}

func New(token string) *Client {
	return &Client{
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

// throttle blocks until at least minRequestInterval has elapsed since the
// previous request. It reserves its slot under the lock, then sleeps unlocked so
// concurrent callers queue rather than collide.
func (c *Client) throttle() {
	c.mu.Lock()
	now := time.Now()
	next := c.lastReq.Add(minRequestInterval)
	if next.After(now) {
		c.lastReq = next
		c.mu.Unlock()
		time.Sleep(next.Sub(now))
		return
	}
	c.lastReq = now
	c.mu.Unlock()
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// ── transport ──────────────────────────────────────────────────────────────

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// do runs a GraphQL request, throttled to respect the rate limit and retried
// with backoff on transient failures (429, 5xx, network errors, empty bodies).
func (c *Client) do(ctx context.Context, query string, vars map[string]any, out any) error {
	const maxAttempts = 4
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt) * 2 * time.Second
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
		c.throttle()
		retryable, err := c.doOnce(ctx, query, vars, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable {
			return err
		}
	}
	return fmt.Errorf("hardcover request failed after %d attempts: %w", maxAttempts, lastErr)
}

// doOnce performs a single attempt. The bool reports whether the error is worth
// retrying.
func (c *Client) doOnce(ctx context.Context, query string, vars map[string]any, out any) (retryable bool, err error) {
	body, err := json.Marshal(gqlRequest{Query: query, Variables: vars})
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return true, fmt.Errorf("graphql: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return true, err
	}

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return true, fmt.Errorf("hardcover http %d: %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("hardcover http %d: %s", resp.StatusCode, snippet(raw))
	}

	var gr gqlResponse
	if err := json.Unmarshal(raw, &gr); err != nil {
		return false, fmt.Errorf("decode response: %w (body: %s)", err, snippet(raw))
	}
	if len(gr.Errors) > 0 {
		return false, fmt.Errorf("graphql error: %s", gr.Errors[0].Message)
	}
	if out != nil {
		if len(gr.Data) == 0 {
			// 200 with no data and no errors — typically a soft throttle; retry.
			return true, fmt.Errorf("empty response body: %s", snippet(raw))
		}
		return false, json.Unmarshal(gr.Data, out)
	}
	return false, nil
}

// ── types ──────────────────────────────────────────────────────────────────

type imageField struct {
	URL string `json:"url"`
}

type publisherField struct {
	Name string `json:"name"`
}

type authorField struct {
	Name string `json:"name"`
}

type contributionField struct {
	Author authorField `json:"author"`
}

type BookInfo struct {
	ID            int64               `json:"id"`
	Title         string              `json:"title"`
	Slug          string              `json:"slug"`
	Contributions []contributionField `json:"contributions"`
}

func (b *BookInfo) AuthorName() string {
	if b != nil && len(b.Contributions) > 0 {
		return b.Contributions[0].Author.Name
	}
	return ""
}

type Edition struct {
	ID              int64           `json:"id"`
	BookID          int64           `json:"book_id"`
	ISBN10          string          `json:"isbn_10"`
	ISBN13          string          `json:"isbn_13"`
	ASIN            string          `json:"asin"`
	ReadingFormatID int             `json:"reading_format_id"`
	AudioSeconds    int             `json:"audio_seconds"`
	Pages           int             `json:"pages"`
	ReleaseYear     int             `json:"release_year"`
	Image           *imageField     `json:"image"`
	Publisher       *publisherField `json:"publisher"`
	Book            *BookInfo       `json:"book"`
}

func (e Edition) ImageURL() string {
	if e.Image != nil {
		return e.Image.URL
	}
	return ""
}

func (e Edition) PublisherName() string {
	if e.Publisher != nil {
		return e.Publisher.Name
	}
	return ""
}

func (e Edition) AuthorName() string {
	if e.Book != nil {
		return e.Book.AuthorName()
	}
	return ""
}

func (e Edition) DisplayTitle() string {
	if e.Book != nil && e.Book.Title != "" {
		return e.Book.Title
	}
	return ""
}

func (e Edition) BookSlug() string {
	if e.Book != nil {
		return e.Book.Slug
	}
	return ""
}

func (e Edition) YearStr() string {
	if e.ReleaseYear > 0 {
		return fmt.Sprintf("%d", e.ReleaseYear)
	}
	return ""
}

func (e Edition) FormatName() string {
	if e.ReadingFormatID == FormatEbook {
		return "Ebook"
	}
	return "Audiobook"
}

// ── queries ────────────────────────────────────────────────────────────────

const editionFields = `
  id book_id isbn_13 isbn_10 asin reading_format_id audio_seconds pages release_year
  image { url }
  publisher { name }
  book {
    id title slug
    contributions(limit: 1) { author { name } }
  }
`

const queryEditionsByISBN = `
query EditionsByISBN($isbn: String!, $format: Int!) {
  editions(where: {_and: [
    {_or: [{isbn_13: {_eq: $isbn}}, {isbn_10: {_eq: $isbn}}]},
    {reading_format_id: {_eq: $format}}
  ]}, limit: 5) {` + editionFields + `}
}`

const queryEditionsByASIN = `
query EditionsByASIN($asin: String!, $format: Int!) {
  editions(where: {_and: [
    {asin: {_eq: $asin}},
    {reading_format_id: {_eq: $format}}
  ]}, limit: 5) {` + editionFields + `}
}`

const queryEditionsByBookID = `
query EditionsByBookID($book_id: Int!) {
  editions(where: {_and: [
    {book_id: {_eq: $book_id}},
    {reading_format_id: {_eq: 2}}
  ]}, limit: 3) {` + editionFields + `}
}`

const queryGetEdition = `
query GetEdition($id: Int!) {
  editions(where: {id: {_eq: $id}}, limit: 1) {` + editionFields + `}
}`

const querySearch = `
query Search($query: String!) {
  search(query: $query, query_type: "books", per_page: 5) {
    results
    error
  }
}`

const queryMe = `query { me { id } }`

const queryGetUserBook = `
query GetUserBook($book_id: Int!, $user_id: Int!) {
  user_books(where: {book_id: {_eq: $book_id}, user_id: {_eq: $user_id}}, limit: 1) {
    id status_id
    user_book_reads(where: {finished_at: {_is_null: true}}, order_by: {id: desc}, limit: 1) {
      id progress_seconds
    }
  }
}`

// ── mutations ──────────────────────────────────────────────────────────────

const mutInsertUserBook = `
mutation InsertUserBook($object: UserBookCreateInput!) {
  insert_user_book(object: $object) {
    id
    user_book { id status_id }
    error
  }
}`

const mutInsertUserBookRead = `
mutation InsertUserBookRead($user_book_id: Int!, $user_book_read: DatesReadInput!) {
  insert_user_book_read(user_book_id: $user_book_id, user_book_read: $user_book_read) {
    id
    error
  }
}`

const mutUpdateUserBookRead = `
mutation UpdateUserBookRead($id: Int!, $object: DatesReadInput!) {
  update_user_book_read(id: $id, object: $object) {
    id
    error
    user_book_read { id progress_seconds }
  }
}`

const mutUpdateUserBookStatus = `
mutation UpdateUserBookStatus($id: Int!, $status_id: Int!) {
  update_user_book(id: $id, object: {status_id: $status_id}) {
    id
    error
  }
}`

// ── search ─────────────────────────────────────────────────────────────────

func (c *Client) SearchByISBN(ctx context.Context, isbn string) ([]Edition, error) {
	var data struct {
		Editions []Edition `json:"editions"`
	}
	if err := c.do(ctx, queryEditionsByISBN, map[string]any{
		"isbn": isbn, "format": FormatAudiobook,
	}, &data); err != nil {
		return nil, err
	}
	return data.Editions, nil
}

func (c *Client) SearchByASIN(ctx context.Context, asin string) ([]Edition, error) {
	var data struct {
		Editions []Edition `json:"editions"`
	}
	if err := c.do(ctx, queryEditionsByASIN, map[string]any{
		"asin": asin, "format": FormatAudiobook,
	}, &data); err != nil {
		return nil, err
	}
	return data.Editions, nil
}

func (c *Client) SearchByTitleAuthor(ctx context.Context, title, author string) ([]Edition, error) {
	query := title
	if author != "" {
		query += " " + author
	}

	var data struct {
		Search struct {
			Results json.RawMessage `json:"results"`
			Error   *string         `json:"error"`
		} `json:"search"`
	}
	if err := c.do(ctx, querySearch, map[string]any{"query": query}, &data); err != nil {
		return nil, err
	}
	if data.Search.Error != nil && *data.Search.Error != "" {
		return nil, fmt.Errorf("search: %s", *data.Search.Error)
	}

	// Hardcover's search returns a Typesense response object, not a flat list:
	// {"found": N, "hits": [{"document": {"id": "<book_id>", ...}}, ...]}.
	// Note: document.id comes back as a STRING, so parse it, don't decode to int.
	var results struct {
		Hits []struct {
			Document struct {
				ID string `json:"id"`
			} `json:"document"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(data.Search.Results, &results); err != nil {
		return nil, fmt.Errorf("parse search results: %w", err)
	}

	var out []Edition
	for _, h := range results.Hits {
		if len(out) >= 5 {
			break
		}
		bookID, err := strconv.ParseInt(h.Document.ID, 10, 64)
		if err != nil {
			continue
		}
		editions, err := c.GetEditionsByBookID(ctx, bookID)
		if err != nil {
			continue
		}
		out = append(out, editions...)
	}
	return out, nil
}

func (c *Client) GetEditionsByBookID(ctx context.Context, bookID int64) ([]Edition, error) {
	var data struct {
		Editions []Edition `json:"editions"`
	}
	if err := c.do(ctx, queryEditionsByBookID, map[string]any{"book_id": bookID}, &data); err != nil {
		return nil, err
	}
	return data.Editions, nil
}

func (c *Client) GetEdition(ctx context.Context, editionID int64) (*Edition, error) {
	var data struct {
		Editions []Edition `json:"editions"`
	}
	if err := c.do(ctx, queryGetEdition, map[string]any{"id": editionID}, &data); err != nil {
		return nil, err
	}
	if len(data.Editions) == 0 {
		return nil, fmt.Errorf("edition %d not found", editionID)
	}
	e := data.Editions[0]
	return &e, nil
}

// ── user book management ───────────────────────────────────────────────────

type UserBookResult struct {
	ID       int64
	StatusID int
	ReadID   *int64
}

func (c *Client) GetUserBook(ctx context.Context, bookID int64) (*UserBookResult, error) {
	var data struct {
		UserBooks []struct {
			ID       int64 `json:"id"`
			StatusID int   `json:"status_id"`
			Reads    []struct {
				ID int64 `json:"id"`
			} `json:"user_book_reads"`
		} `json:"user_books"`
	}
	userID, err := c.CurrentUserID(ctx)
	if err != nil {
		return nil, fmt.Errorf("current user: %w", err)
	}
	if err := c.do(ctx, queryGetUserBook, map[string]any{"book_id": bookID, "user_id": userID}, &data); err != nil {
		return nil, err
	}
	if len(data.UserBooks) == 0 {
		return nil, nil
	}
	ub := data.UserBooks[0]
	r := &UserBookResult{ID: ub.ID, StatusID: ub.StatusID}
	if len(ub.Reads) > 0 {
		id := ub.Reads[0].ID
		r.ReadID = &id
	}
	return r, nil
}

func (c *Client) InsertUserBook(ctx context.Context, bookID, editionID int64, statusID int) (int64, error) {
	var data struct {
		InsertUserBook struct {
			ID    int64   `json:"id"`
			Error *string `json:"error"`
		} `json:"insert_user_book"`
	}
	if err := c.do(ctx, mutInsertUserBook, map[string]any{
		"object": map[string]any{
			"book_id":    bookID,
			"edition_id": editionID,
			"status_id":  statusID,
		},
	}, &data); err != nil {
		return 0, err
	}
	if data.InsertUserBook.Error != nil && *data.InsertUserBook.Error != "" {
		return 0, fmt.Errorf("insert_user_book: %s", *data.InsertUserBook.Error)
	}
	return data.InsertUserBook.ID, nil
}

// ReadProgress is the progress to record on a read, in the unit that matches the
// edition: audiobook editions use Seconds, physical/ebook editions use Pages.
// Exactly one is expected to be non-zero; Hardcover stores them mutually
// exclusively (setting one clears the other).
type ReadProgress struct {
	Seconds int
	Pages   int
}

func (p ReadProgress) isZero() bool { return p.Seconds <= 0 && p.Pages <= 0 }

// apply writes whichever progress unit is set into a DatesReadInput map.
func (p ReadProgress) apply(input map[string]any) {
	if p.Pages > 0 {
		input["progress_pages"] = p.Pages
	} else {
		input["progress_seconds"] = p.Seconds
	}
}

func (c *Client) InsertUserBookRead(ctx context.Context, userBookID, editionID int64, startedAt time.Time, progress ReadProgress, finishedAt *time.Time) (int64, error) {
	var data struct {
		InsertUserBookRead struct {
			ID    int64   `json:"id"`
			Error *string `json:"error"`
		} `json:"insert_user_book_read"`
	}
	readInput := map[string]any{
		"edition_id": editionID,
		"started_at": startedAt.Format("2006-01-02"),
	}
	progress.apply(readInput)
	if finishedAt != nil {
		readInput["finished_at"] = finishedAt.Format("2006-01-02")
	}
	if err := c.do(ctx, mutInsertUserBookRead, map[string]any{
		"user_book_id":   userBookID,
		"user_book_read": readInput,
	}, &data); err != nil {
		return 0, err
	}
	if data.InsertUserBookRead.Error != nil && *data.InsertUserBookRead.Error != "" {
		return 0, fmt.Errorf("insert_user_book_read: %s", *data.InsertUserBookRead.Error)
	}
	readID := data.InsertUserBookRead.ID

	// Hardcover's insert silently drops the progress for an in-progress read
	// (finished_at == null) — only finished reads keep it on insert. So for an
	// unfinished read, set the progress with a follow-up update, which honors it.
	if readID != 0 && finishedAt == nil && !progress.isZero() {
		if err := c.UpdateUserBookRead(ctx, readID, startedAt, progress, nil); err != nil {
			return readID, fmt.Errorf("set progress on new read: %w", err)
		}
	}
	return readID, nil
}

func (c *Client) UpdateUserBookRead(ctx context.Context, readID int64, startedAt time.Time, progress ReadProgress, finishedAt *time.Time) error {
	var data struct {
		UpdateUserBookRead struct {
			ID    int64   `json:"id"`
			Error *string `json:"error"`
		} `json:"update_user_book_read"`
	}
	obj := map[string]any{
		"started_at": startedAt.Format("2006-01-02"),
	}
	progress.apply(obj)
	if finishedAt != nil {
		obj["finished_at"] = finishedAt.Format("2006-01-02")
	}
	if err := c.do(ctx, mutUpdateUserBookRead, map[string]any{
		"id": readID, "object": obj,
	}, &data); err != nil {
		return err
	}
	if data.UpdateUserBookRead.Error != nil && *data.UpdateUserBookRead.Error != "" {
		return fmt.Errorf("update_user_book_read: %s", *data.UpdateUserBookRead.Error)
	}
	return nil
}

// ── my library ────────────────────────────────────────────────────────────

// UserBookSummary is one entry from the user's Hardcover library.
type UserBookSummary struct {
	ID       int64     `json:"id"`
	StatusID int       `json:"status_id"`
	BookID   int64     `json:"book_id"`
	Book     *BookInfo `json:"book"`    // always present; use for title matching
	Edition  *Edition  `json:"edition"` // null when no specific edition was chosen
	Reads    []struct {
		ID              int64   `json:"id"`
		ProgressSeconds float64 `json:"progress_seconds"`
	} `json:"user_book_reads"`
}

func (u UserBookSummary) ActiveReadID() *int64 {
	if len(u.Reads) > 0 {
		id := u.Reads[0].ID
		return &id
	}
	return nil
}

func (u UserBookSummary) ActiveReadProgress() float64 {
	if len(u.Reads) > 0 {
		return u.Reads[0].ProgressSeconds
	}
	return 0
}

// Title returns the best available title for matching.
func (u UserBookSummary) Title() string {
	if u.Book != nil {
		return u.Book.Title
	}
	if u.Edition != nil {
		return u.Edition.DisplayTitle()
	}
	return ""
}

const queryGetMyUserBooks = `
query GetMyUserBooks($user_id: Int!, $limit: Int!, $offset: Int!) {
  user_books(where: {user_id: {_eq: $user_id}}, limit: $limit, offset: $offset, order_by: {created_at: desc}) {
    id
    status_id
    book_id
    book {
      id title
      contributions(limit: 1) { author { name } }
    }
    edition {
      id book_id isbn_13 isbn_10 asin reading_format_id release_year
      image { url }
      publisher { name }
      book {
        id title
        contributions(limit: 1) { author { name } }
      }
    }
    user_book_reads(where: {finished_at: {_is_null: true}}, order_by: {id: desc}, limit: 1) {
      id
      progress_seconds
    }
  }
}`

// CurrentUserID returns the authenticated user's Hardcover ID. The user_books
// table is globally readable, so every "my library" query MUST filter by this —
// without it, queries return other users' shelves.
func (c *Client) CurrentUserID(ctx context.Context) (int64, error) {
	var data struct {
		Me []struct {
			ID int64 `json:"id"`
		} `json:"me"`
	}
	if err := c.do(ctx, queryMe, nil, &data); err != nil {
		return 0, err
	}
	if len(data.Me) == 0 {
		return 0, fmt.Errorf("no authenticated user (check HARDCOVER_TOKEN)")
	}
	return data.Me[0].ID, nil
}

// GetMyUserBooks returns all books in the authenticated user's Hardcover library,
// paginating automatically.
func (c *Client) GetMyUserBooks(ctx context.Context) ([]UserBookSummary, error) {
	userID, err := c.CurrentUserID(ctx)
	if err != nil {
		return nil, fmt.Errorf("current user: %w", err)
	}

	const limit = 250
	var all []UserBookSummary
	offset := 0

	for {
		var data struct {
			UserBooks []UserBookSummary `json:"user_books"`
		}
		if err := c.do(ctx, queryGetMyUserBooks, map[string]any{
			"user_id": userID, "limit": limit, "offset": offset,
		}, &data); err != nil {
			return nil, err
		}
		all = append(all, data.UserBooks...)
		if len(data.UserBooks) < limit {
			break
		}
		offset += limit
	}
	return all, nil
}

func (c *Client) UpdateUserBookStatus(ctx context.Context, userBookID int64, statusID int) error {
	var data struct {
		UpdateUserBook struct {
			ID    int64   `json:"id"`
			Error *string `json:"error"`
		} `json:"update_user_book"`
	}
	if err := c.do(ctx, mutUpdateUserBookStatus, map[string]any{
		"id": userBookID, "status_id": statusID,
	}, &data); err != nil {
		return err
	}
	if data.UpdateUserBook.Error != nil && *data.UpdateUserBook.Error != "" {
		return fmt.Errorf("update_user_book: %s", *data.UpdateUserBook.Error)
	}
	return nil
}
