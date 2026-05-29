package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// BookStatus is the sync state of a book.
type BookStatus string

const (
	StatusUnmatched BookStatus = "unmatched"
	StatusMatched   BookStatus = "matched"
	StatusIgnored   BookStatus = "ignored"
)

func (s BookStatus) Valid() bool {
	switch s {
	case StatusUnmatched, StatusMatched, StatusIgnored:
		return true
	}
	return false
}

const schema = `
CREATE TABLE IF NOT EXISTS books (
	id                        INTEGER PRIMARY KEY AUTOINCREMENT,
	abs_item_id               TEXT    NOT NULL UNIQUE,
	abs_title                 TEXT    NOT NULL DEFAULT '',
	abs_author                TEXT    NOT NULL DEFAULT '',
	abs_isbn                  TEXT    NOT NULL DEFAULT '',
	abs_asin                  TEXT    NOT NULL DEFAULT '',
	abs_added_at              DATETIME,
	abs_total_seconds         REAL    NOT NULL DEFAULT 0,
	abs_current_seconds       REAL    NOT NULL DEFAULT 0,
	abs_is_finished           INTEGER NOT NULL DEFAULT 0,
	abs_last_seen_at          DATETIME,

	hc_edition_id             INTEGER,
	hc_book_id                INTEGER,
	hc_user_book_id           INTEGER,
	hc_user_book_read_id      INTEGER,
	hc_edition_title          TEXT    NOT NULL DEFAULT '',
	hc_edition_publisher      TEXT    NOT NULL DEFAULT '',
	hc_edition_year           TEXT    NOT NULL DEFAULT '',
	hc_edition_format         TEXT    NOT NULL DEFAULT '',
	hc_edition_image_url      TEXT    NOT NULL DEFAULT '',

	status                    TEXT    NOT NULL DEFAULT 'unmatched',
	last_synced_seconds       REAL    NOT NULL DEFAULT 0,
	last_synced_is_finished   INTEGER NOT NULL DEFAULT 0,
	last_synced_at            DATETIME,
	pending_reread            INTEGER NOT NULL DEFAULT 0,

	candidate_edition_id      INTEGER,
	candidate_book_id         INTEGER,
	candidate_title           TEXT    NOT NULL DEFAULT '',
	candidate_author          TEXT    NOT NULL DEFAULT '',
	candidate_publisher       TEXT    NOT NULL DEFAULT '',
	candidate_year            TEXT    NOT NULL DEFAULT '',
	candidate_image_url       TEXT    NOT NULL DEFAULT '',
	candidate_reason          TEXT    NOT NULL DEFAULT '',

	created_at                DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at                DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TRIGGER IF NOT EXISTS books_updated_at
AFTER UPDATE ON books FOR EACH ROW
BEGIN
	UPDATE books SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
END;
`

// migrations run at startup; errors are ignored (column may already exist).
var migrations = []string{
	`ALTER TABLE books ADD COLUMN abs_total_seconds REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE books ADD COLUMN abs_added_at DATETIME`,
}

type DB struct {
	sql *sql.DB
}

func Open(path string) (*DB, error) {
	dsn := path + "?_journal_mode=WAL&_foreign_keys=on"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)

	if _, err := sqlDB.Exec(schema); err != nil {
		return nil, fmt.Errorf("init schema: %w", err)
	}
	for _, m := range migrations {
		_, _ = sqlDB.Exec(m) // ignore "duplicate column" error on re-runs
	}

	return &DB{sql: sqlDB}, nil
}

func (d *DB) Close() error { return d.sql.Close() }

// Book is the central data type representing a synced book.
type Book struct {
	ID            int64
	ABSItemID     string
	ABSTitle      string
	ABSAuthor     string
	ABSISBN       string
	ABSASIN       string
	ABSAddedAt         *time.Time
	ABSTotalSeconds    float64
	ABSCurrentSeconds  float64
	ABSIsFinished      bool
	ABSLastSeenAt      *time.Time

	HCEditionID        *int64
	HCBookID           *int64
	HCUserBookID       *int64
	HCUserBookReadID   *int64
	HCEditionTitle     string
	HCEditionPublisher string
	HCEditionYear      string
	HCEditionFormat    string
	HCEditionImageURL  string

	Status               BookStatus
	LastSyncedSeconds    float64
	LastSyncedIsFinished bool
	LastSyncedAt         *time.Time
	PendingReread        bool

	CandidateEditionID  *int64
	CandidateBookID     *int64
	CandidateTitle      string
	CandidateAuthor     string
	CandidatePublisher  string
	CandidateYear       string
	CandidateImageURL   string
	CandidateReason     string

	CreatedAt time.Time
	UpdatedAt time.Time
}

func (b Book) NeedsSync() bool {
	if b.Status != StatusMatched {
		return false
	}
	if b.ABSIsFinished && !b.LastSyncedIsFinished {
		return true
	}
	return b.ABSCurrentSeconds-b.LastSyncedSeconds >= 120
}

type BooksByCategory struct {
	Unmatched []Book
	NeedsSync []Book
	Synced    []Book
	Ignored   []Book
}

const selectAll = `
SELECT
	id, abs_item_id, abs_title, abs_author, abs_isbn, abs_asin,
	abs_added_at, abs_total_seconds, abs_current_seconds, abs_is_finished, abs_last_seen_at,
	hc_edition_id, hc_book_id, hc_user_book_id, hc_user_book_read_id,
	hc_edition_title, hc_edition_publisher, hc_edition_year, hc_edition_format, hc_edition_image_url,
	status, last_synced_seconds, last_synced_is_finished, last_synced_at, pending_reread,
	candidate_edition_id, candidate_book_id, candidate_title, candidate_author,
	candidate_publisher, candidate_year, candidate_image_url, candidate_reason,
	created_at, updated_at
FROM books
`

func scanBook(row interface{ Scan(...any) error }) (Book, error) {
	var b Book
	var absAddedAt, absLastSeen, lastSyncedAt sql.NullTime
	var absIsFinished, lastSyncedIsFinished, pendingReread int
	var status string

	err := row.Scan(
		&b.ID, &b.ABSItemID, &b.ABSTitle, &b.ABSAuthor, &b.ABSISBN, &b.ABSASIN,
		&absAddedAt, &b.ABSTotalSeconds, &b.ABSCurrentSeconds, &absIsFinished, &absLastSeen,
		&b.HCEditionID, &b.HCBookID, &b.HCUserBookID, &b.HCUserBookReadID,
		&b.HCEditionTitle, &b.HCEditionPublisher, &b.HCEditionYear, &b.HCEditionFormat, &b.HCEditionImageURL,
		&status, &b.LastSyncedSeconds, &lastSyncedIsFinished, &lastSyncedAt, &pendingReread,
		&b.CandidateEditionID, &b.CandidateBookID, &b.CandidateTitle, &b.CandidateAuthor,
		&b.CandidatePublisher, &b.CandidateYear, &b.CandidateImageURL, &b.CandidateReason,
		&b.CreatedAt, &b.UpdatedAt,
	)
	if err != nil {
		return Book{}, err
	}

	b.Status = BookStatus(status)
	b.ABSIsFinished = absIsFinished != 0
	if absAddedAt.Valid {
		b.ABSAddedAt = &absAddedAt.Time
	}
	b.LastSyncedIsFinished = lastSyncedIsFinished != 0
	b.PendingReread = pendingReread != 0
	if absLastSeen.Valid {
		b.ABSLastSeenAt = &absLastSeen.Time
	}
	if lastSyncedAt.Valid {
		b.LastSyncedAt = &lastSyncedAt.Time
	}
	return b, nil
}

func (d *DB) GetBook(ctx context.Context, id int64) (Book, error) {
	row := d.sql.QueryRowContext(ctx, selectAll+" WHERE id = ?", id)
	b, err := scanBook(row)
	if err == sql.ErrNoRows {
		return Book{}, fmt.Errorf("book %d not found", id)
	}
	return b, err
}

func (d *DB) ListAllBooks(ctx context.Context) ([]Book, error) {
	rows, err := d.sql.QueryContext(ctx, selectAll+" ORDER BY abs_added_at DESC, created_at DESC, id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var books []Book
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			return nil, err
		}
		books = append(books, b)
	}
	return books, rows.Err()
}

func (d *DB) ListBooksByCategory(ctx context.Context) (BooksByCategory, error) {
	books, err := d.ListAllBooks(ctx)
	if err != nil {
		return BooksByCategory{}, err
	}

	var cats BooksByCategory
	for _, b := range books {
		switch {
		case b.Status == StatusIgnored:
			cats.Ignored = append(cats.Ignored, b)
		case b.Status == StatusUnmatched:
			cats.Unmatched = append(cats.Unmatched, b)
		case b.NeedsSync():
			cats.NeedsSync = append(cats.NeedsSync, b)
		default:
			cats.Synced = append(cats.Synced, b)
		}
	}
	return cats, nil
}

func (d *DB) UpsertABSBook(ctx context.Context, absItemID, title, author, isbn, asin string, addedAt time.Time, totalSeconds float64) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO books (abs_item_id, abs_title, abs_author, abs_isbn, abs_asin, abs_added_at, abs_total_seconds)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(abs_item_id) DO UPDATE SET
			abs_title         = excluded.abs_title,
			abs_author        = excluded.abs_author,
			abs_isbn          = CASE WHEN excluded.abs_isbn != '' THEN excluded.abs_isbn ELSE abs_isbn END,
			abs_asin          = CASE WHEN excluded.abs_asin != '' THEN excluded.abs_asin ELSE abs_asin END,
			abs_added_at      = CASE WHEN excluded.abs_added_at IS NOT NULL THEN excluded.abs_added_at ELSE abs_added_at END,
			abs_total_seconds = CASE WHEN excluded.abs_total_seconds > 0 THEN excluded.abs_total_seconds ELSE abs_total_seconds END
	`, absItemID, title, author, isbn, asin, addedAt, totalSeconds)
	return err
}

func (d *DB) UpdateABSProgress(ctx context.Context, absItemID string, currentSeconds float64, isFinished bool, lastSeenAt time.Time) error {
	fin := 0
	if isFinished {
		fin = 1
	}
	_, err := d.sql.ExecContext(ctx, `
		UPDATE books SET
			abs_current_seconds = ?,
			abs_is_finished     = ?,
			abs_last_seen_at    = ?
		WHERE abs_item_id = ?
	`, currentSeconds, fin, lastSeenAt, absItemID)
	return err
}

func (d *DB) SetStatus(ctx context.Context, id int64, status BookStatus) error {
	if !status.Valid() {
		return fmt.Errorf("invalid status %q", status)
	}
	_, err := d.sql.ExecContext(ctx, `UPDATE books SET status = ? WHERE id = ?`, string(status), id)
	return err
}

type MatchCandidate struct {
	EditionID  int64
	BookID     int64
	Title      string
	Author     string
	Publisher  string
	Year       string
	ImageURL   string
	Reason     string
}

func (d *DB) SetMatchCandidate(ctx context.Context, bookID int64, c MatchCandidate) error {
	_, err := d.sql.ExecContext(ctx, `
		UPDATE books SET
			candidate_edition_id = ?,
			candidate_book_id    = ?,
			candidate_title      = ?,
			candidate_author     = ?,
			candidate_publisher  = ?,
			candidate_year       = ?,
			candidate_image_url  = ?,
			candidate_reason     = ?
		WHERE id = ?
	`, c.EditionID, c.BookID, c.Title, c.Author, c.Publisher, c.Year, c.ImageURL, c.Reason, bookID)
	return err
}

type HCMatch struct {
	EditionID      int64
	BookID         int64
	UserBookID     int64
	UserBookReadID int64
	EditionTitle   string
	Publisher      string
	Year           string
	Format         string
	ImageURL       string
}

func (d *DB) SetMatch(ctx context.Context, bookID int64, m HCMatch) error {
	_, err := d.sql.ExecContext(ctx, `
		UPDATE books SET
			hc_edition_id        = ?,
			hc_book_id           = ?,
			hc_user_book_id      = ?,
			hc_user_book_read_id = ?,
			hc_edition_title     = ?,
			hc_edition_publisher = ?,
			hc_edition_year      = ?,
			hc_edition_format    = ?,
			hc_edition_image_url = ?,
			status               = 'matched',
			candidate_edition_id = NULL,
			candidate_book_id    = NULL,
			candidate_title      = '',
			candidate_author     = '',
			candidate_publisher  = '',
			candidate_year       = '',
			candidate_image_url  = '',
			candidate_reason     = ''
		WHERE id = ?
	`, m.EditionID, m.BookID, m.UserBookID, m.UserBookReadID,
		m.EditionTitle, m.Publisher, m.Year, m.Format, m.ImageURL, bookID)
	return err
}

func (d *DB) UpdateLastSynced(ctx context.Context, bookID int64, seconds float64, isFinished bool, at time.Time) error {
	fin := 0
	if isFinished {
		fin = 1
	}
	_, err := d.sql.ExecContext(ctx, `
		UPDATE books SET
			last_synced_seconds     = ?,
			last_synced_is_finished = ?,
			last_synced_at          = ?
		WHERE id = ?
	`, seconds, fin, at, bookID)
	return err
}

func (d *DB) SetPendingReread(ctx context.Context, bookID int64, pending bool) error {
	v := 0
	if pending {
		v = 1
	}
	_, err := d.sql.ExecContext(ctx, `UPDATE books SET pending_reread = ? WHERE id = ?`, v, bookID)
	return err
}

func (d *DB) SetHCUserBookReadID(ctx context.Context, bookID int64, readID int64) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE books SET hc_user_book_read_id = ? WHERE id = ?`, readID, bookID)
	return err
}
