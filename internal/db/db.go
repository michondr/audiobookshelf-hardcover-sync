package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS books (
	id                   INTEGER PRIMARY KEY AUTOINCREMENT,
	abs_item_id          TEXT    NOT NULL UNIQUE,
	abs_title            TEXT    NOT NULL DEFAULT '',
	abs_author           TEXT    NOT NULL DEFAULT '',
	abs_isbn             TEXT    NOT NULL DEFAULT '',
	abs_asin             TEXT    NOT NULL DEFAULT '',
	abs_added_at         DATETIME,
	abs_total_seconds    REAL    NOT NULL DEFAULT 0,
	abs_current_seconds  REAL    NOT NULL DEFAULT 0,
	abs_is_finished      INTEGER NOT NULL DEFAULT 0,
	abs_last_seen_at     DATETIME,
	abs_started_at       DATETIME,
	abs_finished_at      DATETIME,
	hc_edition_id        INTEGER,
	hc_book_id           INTEGER,
	hc_ignored           INTEGER NOT NULL DEFAULT 0,
	hc_candidates_json   TEXT    NOT NULL DEFAULT '',
	hc_edition_data_json TEXT    NOT NULL DEFAULT '',
	hc_match_searched_at DATETIME,
	hc_current_seconds   REAL    NOT NULL DEFAULT 0,
	hc_is_finished       INTEGER NOT NULL DEFAULT 0,
	hc_dnf               INTEGER NOT NULL DEFAULT 0,
	hc_progress_synced_at DATETIME,
	created_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TRIGGER IF NOT EXISTS books_updated_at
AFTER UPDATE ON books FOR EACH ROW
BEGIN
	UPDATE books SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
END;
`

// migrations run at startup; errors are ignored (column may already exist / not exist).
//
// IMPORTANT: every statement here must be idempotent AND non-destructive, because
// it runs on every boot. ADD COLUMN is safe (ignored if the column exists). DROP
// COLUMN is only safe for columns that no longer belong to the current schema —
// never drop a live column, or its data is wiped on every restart.
var migrations = []string{
	// additive migrations for current columns (idempotent: ignored if column exists)
	`ALTER TABLE books ADD COLUMN abs_total_seconds REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE books ADD COLUMN abs_added_at DATETIME`,
	`ALTER TABLE books ADD COLUMN abs_started_at DATETIME`,
	`ALTER TABLE books ADD COLUMN abs_finished_at DATETIME`,
	`ALTER TABLE books ADD COLUMN hc_edition_id INTEGER`,
	`ALTER TABLE books ADD COLUMN hc_book_id INTEGER`,
	`ALTER TABLE books ADD COLUMN hc_ignored INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE books ADD COLUMN hc_candidates_json TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE books ADD COLUMN hc_edition_data_json TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE books ADD COLUMN hc_match_searched_at DATETIME`,
	`ALTER TABLE books ADD COLUMN hc_current_seconds REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE books ADD COLUMN hc_is_finished INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE books ADD COLUMN hc_dnf INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE books ADD COLUMN hc_progress_synced_at DATETIME`,
	// one-time cleanup of columns removed in the refactor (no-op once gone)
	`ALTER TABLE books DROP COLUMN hc_user_book_id`,
	`ALTER TABLE books DROP COLUMN hc_user_book_read_id`,
	`ALTER TABLE books DROP COLUMN hc_edition_title`,
	`ALTER TABLE books DROP COLUMN hc_edition_publisher`,
	`ALTER TABLE books DROP COLUMN hc_edition_year`,
	`ALTER TABLE books DROP COLUMN hc_edition_format`,
	`ALTER TABLE books DROP COLUMN hc_edition_image_url`,
	`ALTER TABLE books DROP COLUMN status`,
	`ALTER TABLE books DROP COLUMN last_synced_seconds`,
	`ALTER TABLE books DROP COLUMN last_synced_is_finished`,
	`ALTER TABLE books DROP COLUMN last_synced_at`,
	`ALTER TABLE books DROP COLUMN pending_reread`,
	`ALTER TABLE books DROP COLUMN candidate_edition_id`,
	`ALTER TABLE books DROP COLUMN candidate_book_id`,
	`ALTER TABLE books DROP COLUMN candidate_title`,
	`ALTER TABLE books DROP COLUMN candidate_author`,
	`ALTER TABLE books DROP COLUMN candidate_publisher`,
	`ALTER TABLE books DROP COLUMN candidate_year`,
	`ALTER TABLE books DROP COLUMN candidate_image_url`,
	`ALTER TABLE books DROP COLUMN candidate_reason`,
	`ALTER TABLE books DROP COLUMN candidates_json`,
	// self-heal: restore edition/book IDs wiped by the old destructive migration,
	// reconstructing them from the cached edition JSON (idempotent).
	`UPDATE books
	    SET hc_edition_id = json_extract(hc_edition_data_json, '$.id'),
	        hc_book_id    = json_extract(hc_edition_data_json, '$.book_id')
	  WHERE hc_edition_id IS NULL
	    AND hc_edition_data_json != ''
	    AND json_valid(hc_edition_data_json)`,
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
		_, _ = sqlDB.Exec(m) // ignore "duplicate column" / "no such column" errors on re-runs
	}

	return &DB{sql: sqlDB}, nil
}

func (d *DB) Close() error { return d.sql.Close() }

// CandidateEdition holds the edition data we cache locally for display and matching.
type CandidateEdition struct {
	ID        int64  `json:"id"`
	BookID    int64  `json:"book_id"`
	Title     string `json:"title"`
	Author    string `json:"author"`
	Publisher string `json:"publisher"`
	Year      int    `json:"year"`
	FormatID  int    `json:"format_id"`
	ImageURL  string `json:"image_url"`
	ISBN13    string `json:"isbn_13"`
	ASIN      string `json:"asin"`
	Slug      string `json:"slug"`
}

func (c CandidateEdition) FormatName() string {
	if c.FormatID == 4 {
		return "Ebook"
	}
	return "Audiobook"
}

type Book struct {
	ID                  int64
	ABSItemID           string
	ABSTitle            string
	ABSAuthor           string
	ABSISBN             string
	ABSASIN             string
	ABSAddedAt          *time.Time
	ABSTotalSeconds     float64
	ABSCurrentSeconds   float64
	ABSIsFinished       bool
	ABSLastSeenAt       *time.Time
	ABSStartedAt        *time.Time
	ABSFinishedAt       *time.Time
	HCEditionID         *int64
	HCBookID            *int64
	HCIgnored           bool
	HCCandidatesJSON    string
	HCEditionDataJSON   string
	HCMatchSearchedAt   *time.Time
	HCCurrentSeconds    float64
	HCIsFinished        bool
	HCDNF               bool
	HCProgressSyncedAt  *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

func (b Book) ParsedCandidates() []CandidateEdition {
	if b.HCCandidatesJSON == "" {
		return nil
	}
	var out []CandidateEdition
	_ = json.Unmarshal([]byte(b.HCCandidatesJSON), &out)
	return out
}

func (b Book) ParsedEdition() *CandidateEdition {
	if b.HCEditionDataJSON == "" {
		return nil
	}
	var out CandidateEdition
	if err := json.Unmarshal([]byte(b.HCEditionDataJSON), &out); err != nil {
		return nil
	}
	return &out
}

const selectAll = `
SELECT
	id, abs_item_id, abs_title, abs_author, abs_isbn, abs_asin,
	abs_added_at, abs_total_seconds, abs_current_seconds, abs_is_finished,
	abs_last_seen_at, abs_started_at, abs_finished_at,
	hc_edition_id, hc_book_id, hc_ignored, hc_candidates_json, hc_edition_data_json, hc_match_searched_at,
	hc_current_seconds, hc_is_finished, hc_dnf, hc_progress_synced_at,
	created_at, updated_at
FROM books
`

func scanBook(row interface{ Scan(...any) error }) (Book, error) {
	var b Book
	var absAddedAt, absLastSeen, absStartedAt, absFinishedAt sql.NullTime
	var absIsFinished, hcIgnored, hcIsFinished, hcDNF int
	var hcEditionID, hcBookID sql.NullInt64
	var hcMatchSearchedAt, hcProgressSyncedAt sql.NullTime

	err := row.Scan(
		&b.ID, &b.ABSItemID, &b.ABSTitle, &b.ABSAuthor, &b.ABSISBN, &b.ABSASIN,
		&absAddedAt, &b.ABSTotalSeconds, &b.ABSCurrentSeconds, &absIsFinished,
		&absLastSeen, &absStartedAt, &absFinishedAt,
		&hcEditionID, &hcBookID, &hcIgnored, &b.HCCandidatesJSON, &b.HCEditionDataJSON, &hcMatchSearchedAt,
		&b.HCCurrentSeconds, &hcIsFinished, &hcDNF, &hcProgressSyncedAt,
		&b.CreatedAt, &b.UpdatedAt,
	)
	if err != nil {
		return Book{}, err
	}

	b.ABSIsFinished = absIsFinished != 0
	b.HCIgnored = hcIgnored != 0
	b.HCIsFinished = hcIsFinished != 0
	b.HCDNF = hcDNF != 0
	if absAddedAt.Valid {
		b.ABSAddedAt = &absAddedAt.Time
	}
	if absLastSeen.Valid {
		b.ABSLastSeenAt = &absLastSeen.Time
	}
	if absStartedAt.Valid {
		b.ABSStartedAt = &absStartedAt.Time
	}
	if absFinishedAt.Valid {
		b.ABSFinishedAt = &absFinishedAt.Time
	}
	if hcEditionID.Valid {
		b.HCEditionID = &hcEditionID.Int64
	}
	if hcBookID.Valid {
		b.HCBookID = &hcBookID.Int64
	}
	if hcMatchSearchedAt.Valid {
		b.HCMatchSearchedAt = &hcMatchSearchedAt.Time
	}
	if hcProgressSyncedAt.Valid {
		b.HCProgressSyncedAt = &hcProgressSyncedAt.Time
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

func (d *DB) ListUnmatchedBooks(ctx context.Context) ([]Book, error) {
	rows, err := d.sql.QueryContext(ctx, selectAll+`
		WHERE hc_edition_id IS NULL
		  AND hc_ignored = 0
		  AND hc_match_searched_at IS NULL
		ORDER BY abs_added_at DESC, created_at DESC, id DESC
	`)
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

// ListMatchedBooks returns every book confirmed to a Hardcover edition (and not
// ignored), so a progress pass can compare ABS vs Hardcover reading progress.
func (d *DB) ListMatchedBooks(ctx context.Context) ([]Book, error) {
	rows, err := d.sql.QueryContext(ctx, selectAll+`
		WHERE hc_edition_id IS NOT NULL
		  AND hc_ignored = 0
		ORDER BY abs_added_at DESC, created_at DESC, id DESC
	`)
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

// UpdateHCProgress records the reading progress a matched book currently has on
// Hardcover, so the UI can flag books whose ABS progress has drifted from it.
func (d *DB) UpdateHCProgress(ctx context.Context, id int64, currentSeconds float64, isFinished, dnf bool) error {
	fin := 0
	if isFinished {
		fin = 1
	}
	dnfVal := 0
	if dnf {
		dnfVal = 1
	}
	_, err := d.sql.ExecContext(ctx, `
		UPDATE books SET
			hc_current_seconds    = ?,
			hc_is_finished        = ?,
			hc_dnf                = ?,
			hc_progress_synced_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, currentSeconds, fin, dnfVal, id)
	return err
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

func (d *DB) UpdateABSProgress(ctx context.Context, absItemID string, currentSeconds float64, isFinished bool, lastSeenAt time.Time, startedAt, finishedAt *time.Time) error {
	fin := 0
	if isFinished {
		fin = 1
	}
	_, err := d.sql.ExecContext(ctx, `
		UPDATE books SET
			abs_current_seconds = ?,
			abs_is_finished     = ?,
			abs_last_seen_at    = ?,
			abs_started_at      = CASE WHEN ? IS NOT NULL THEN ? ELSE abs_started_at END,
			abs_finished_at     = CASE WHEN ? IS NOT NULL THEN ? ELSE abs_finished_at END
		WHERE abs_item_id = ?
	`, currentSeconds, fin, lastSeenAt, startedAt, startedAt, finishedAt, finishedAt, absItemID)
	return err
}

func (d *DB) SetHCEdition(ctx context.Context, id int64, e CandidateEdition) error {
	data, _ := json.Marshal(e)
	_, err := d.sql.ExecContext(ctx, `
		UPDATE books SET
			hc_edition_id         = ?,
			hc_book_id            = ?,
			hc_edition_data_json  = ?,
			hc_candidates_json    = '',
			hc_match_searched_at  = CURRENT_TIMESTAMP,
			hc_current_seconds    = 0,
			hc_is_finished        = 0,
			hc_dnf                = 0,
			hc_progress_synced_at = NULL
		WHERE id = ?
	`, e.ID, e.BookID, string(data), id)
	return err
}

func (d *DB) SetHCCandidates(ctx context.Context, id int64, candidates []CandidateEdition) error {
	data, _ := json.Marshal(candidates)
	_, err := d.sql.ExecContext(ctx, `
		UPDATE books SET
			hc_edition_id        = NULL,
			hc_book_id           = NULL,
			hc_edition_data_json = '',
			hc_candidates_json   = ?,
			hc_match_searched_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, string(data), id)
	return err
}

func (d *DB) SetHCMatchSearched(ctx context.Context, id int64) error {
	_, err := d.sql.ExecContext(ctx, `
		UPDATE books SET hc_match_searched_at = CURRENT_TIMESTAMP WHERE id = ?
	`, id)
	return err
}

// ResetUnmatchedSearch clears the "searched" marker (and any stale candidates)
// on every book that isn't matched or ignored, so the next match pass
// reconsiders them from scratch. Used to re-run matching after a logic change.
func (d *DB) ResetUnmatchedSearch(ctx context.Context) error {
	_, err := d.sql.ExecContext(ctx, `
		UPDATE books SET
			hc_match_searched_at = NULL,
			hc_candidates_json   = ''
		WHERE hc_edition_id IS NULL AND hc_ignored = 0
	`)
	return err
}

// UnmatchBook clears a confirmed match and resets the book so the next sync
// re-matches it from scratch.
func (d *DB) UnmatchBook(ctx context.Context, id int64) error {
	_, err := d.sql.ExecContext(ctx, `
		UPDATE books SET
			hc_edition_id         = NULL,
			hc_book_id            = NULL,
			hc_edition_data_json  = '',
			hc_candidates_json    = '',
			hc_match_searched_at  = NULL,
			hc_current_seconds    = 0,
			hc_is_finished        = 0,
			hc_dnf                = 0,
			hc_progress_synced_at = NULL
		WHERE id = ?
	`, id)
	return err
}

func (d *DB) SetHCIgnored(ctx context.Context, id int64, ignored bool) error {
	v := 0
	if ignored {
		v = 1
	}
	_, err := d.sql.ExecContext(ctx, `UPDATE books SET hc_ignored = ? WHERE id = ?`, v, id)
	return err
}

func (d *DB) UnignoreBook(ctx context.Context, id int64) error {
	_, err := d.sql.ExecContext(ctx, `
		UPDATE books SET hc_ignored = 0, hc_match_searched_at = NULL WHERE id = ?
	`, id)
	return err
}
