package sync

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/michondr/audiobookshelf-hardcover-sync/internal/abs"
	"github.com/michondr/audiobookshelf-hardcover-sync/internal/db"
	"github.com/michondr/audiobookshelf-hardcover-sync/internal/hardcover"
)

type Service struct {
	db  *db.DB
	abs *abs.Client
	hc  *hardcover.Client
	log *slog.Logger

	matching atomic.Bool // true while a background match pass is running
}

// Matching reports whether a background match pass is currently running.
func (s *Service) Matching() bool { return s.matching.Load() }

// MatchUnmatchedInBackground starts a match pass in a goroutine unless one is
// already running. Returns true if it started a new pass. It uses its own
// context so it survives the HTTP request that triggered it.
func (s *Service) MatchUnmatchedInBackground() bool {
	if !s.matching.CompareAndSwap(false, true) {
		return false
	}
	go func() {
		defer s.matching.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := s.MatchUnmatched(ctx); err != nil {
			s.log.Error("background match", "err", err)
		}
		if err := s.RefreshHCProgress(ctx); err != nil {
			s.log.Error("background HC progress refresh", "err", err)
		}
	}()
	return true
}

func New(database *db.DB, absClient *abs.Client, hcClient *hardcover.Client, log *slog.Logger) *Service {
	return &Service{db: database, abs: absClient, hc: hcClient, log: log}
}

// RefreshFromABS fetches all books from ABS and upserts them into the DB.
func (s *Service) RefreshFromABS(ctx context.Context) error {
	books, err := s.abs.GetAllBooks(ctx)
	if err != nil {
		return fmt.Errorf("refresh from ABS: %w", err)
	}

	for _, b := range books {
		if err := s.db.UpsertABSBook(ctx, b.ItemID, b.Title, b.Author, b.ISBN, b.ASIN, b.AddedAt, b.TotalSeconds); err != nil {
			s.log.Error("upsert book", "item_id", b.ItemID, "err", err)
			continue
		}

		// b.LastUpdate is the ABS progress lastUpdate (when last played). Leave it
		// nil for never-played books so it doesn't get stamped with the fetch time.
		var lastPlayed *time.Time
		if !b.LastUpdate.IsZero() {
			lastPlayed = &b.LastUpdate
		}
		if err := s.db.UpdateABSProgress(ctx, b.ItemID, b.CurrentSeconds, b.IsFinished, lastPlayed, b.StartedAt, b.FinishedAt); err != nil {
			s.log.Error("update progress", "item_id", b.ItemID, "err", err)
		}
	}

	return nil
}

// MatchUnmatched matches every not-yet-matched ABS book to a Hardcover edition.
//
// For each book it tries, in order:
//  1. the user's existing Hardcover library (by ISBN/ASIN, then title+author) —
//     an exact hit auto-matches to that book's edition;
//  2. a Hardcover catalog search (ISBN → ASIN → title+author): one result
//     auto-matches, several become candidates to pick from, none marks the book
//     "searched" so the UI can offer a manual edition-ID input.
func (s *Service) MatchUnmatched(ctx context.Context) error {
	books, err := s.db.ListUnmatchedBooks(ctx)
	if err != nil {
		return fmt.Errorf("list unmatched: %w", err)
	}
	if len(books) == 0 {
		return nil
	}

	// Load the user's existing Hardcover library once so we can match against
	// books they already track before reaching for the catalog. A failure here
	// is non-fatal: we just fall back to catalog-only matching.
	lib, err := s.loadLibraryIndex(ctx)
	if err != nil {
		s.log.Warn("load HC library", "err", err)
	}

	var matched, viaLibrary, candidates, notFound int
	for _, book := range books {
		// 1. Already in the user's Hardcover library?
		if lib != nil {
			if cand := s.matchInLibrary(ctx, book, lib); cand != nil {
				if err := s.db.SetHCEdition(ctx, book.ID, *cand); err != nil {
					s.log.Error("set edition (library)", "book_id", book.ID, "err", err)
				}
				matched++
				viaLibrary++
				continue
			}
		}

		// 2. Fall back to a Hardcover catalog search.
		found, err := s.searchHC(ctx, book)
		if err != nil {
			s.log.Warn("HC search failed", "book_id", book.ID, "title", book.ABSTitle, "err", err)
			_ = s.db.SetHCMatchSearched(ctx, book.ID)
			notFound++
			continue
		}

		switch len(found) {
		case 0:
			if err := s.db.SetHCMatchSearched(ctx, book.ID); err != nil {
				s.log.Error("set searched", "book_id", book.ID, "err", err)
			}
			notFound++
		case 1:
			if err := s.db.SetHCEdition(ctx, book.ID, found[0]); err != nil {
				s.log.Error("set edition", "book_id", book.ID, "err", err)
			}
			matched++
		default:
			if err := s.db.SetHCCandidates(ctx, book.ID, found); err != nil {
				s.log.Error("set candidates", "book_id", book.ID, "err", err)
			}
			candidates++
		}
	}
	s.log.Info("match pass complete",
		"books", len(books), "matched", matched, "via_library", viaLibrary,
		"candidates", candidates, "not_found", notFound)
	return nil
}

// RefreshHCProgress pulls the current reading progress from Hardcover for every
// matched book and stores it, so the UI can flag books whose ABS progress has
// drifted from Hardcover. It loads the user's whole library once and maps it by
// Hardcover book ID. A matched book absent from the library (e.g. matched from
// the catalog but never added to a shelf) records zero progress — which is
// correct: ABS is ahead and the book still needs pushing to Hardcover.
func (s *Service) RefreshHCProgress(ctx context.Context) error {
	matched, err := s.db.ListMatchedBooks(ctx)
	if err != nil {
		return fmt.Errorf("list matched: %w", err)
	}
	if len(matched) == 0 {
		return nil
	}

	lib, err := s.hc.GetMyUserBooks(ctx)
	if err != nil {
		return fmt.Errorf("load HC library: %w", err)
	}
	byBook := make(map[int64]hardcover.UserBookSummary, len(lib))
	for _, ub := range lib {
		byBook[ub.BookID] = ub
	}

	for _, book := range matched {
		if book.HCBookID == nil {
			continue
		}
		var seconds float64
		var finished, dnf bool
		if ub, ok := byBook[*book.HCBookID]; ok {
			finished = ub.StatusID == hardcover.StatusRead
			dnf = ub.StatusID == hardcover.StatusDidNotFinish
			seconds = ub.ActiveReadProgress()
			// Page-based editions (no audio length) record progress_pages, not
			// seconds. Convert it back to equivalent ABS seconds so drift
			// detection compares like with like instead of always seeing 0.
			if seconds <= 0 && book.HCEditionID != nil {
				if pages := ub.ActiveReadProgressPages(); pages > 0 && book.ABSTotalSeconds > 0 {
					if ed, err := s.hc.GetEdition(ctx, *book.HCEditionID); err == nil && ed.Pages > 0 {
						seconds = pages / float64(ed.Pages) * book.ABSTotalSeconds
					}
				}
			}
		}
		if err := s.db.UpdateHCProgress(ctx, book.ID, seconds, finished, dnf); err != nil {
			s.log.Error("update HC progress", "book_id", book.ID, "err", err)
		}
	}
	return nil
}

// PushProgress writes a single matched book's current ABS progress to
// Hardcover. With reread=false it updates the most recent read in place
// (creating one only if the book has none yet); with reread=true it always
// inserts a fresh read. The read's started_at mirrors the ABS start date so
// Hardcover reflects when the listen actually began, and the book's status is
// set to Read/Currently-reading from the ABS finished flag.
func (s *Service) PushProgress(ctx context.Context, bookID int64, reread bool) error {
	book, err := s.db.GetBook(ctx, bookID)
	if err != nil {
		return fmt.Errorf("get book: %w", err)
	}
	if book.HCBookID == nil || book.HCEditionID == nil {
		return fmt.Errorf("book is not matched to a Hardcover edition")
	}

	startedAt := time.Now()
	if book.ABSStartedAt != nil {
		startedAt = *book.ABSStartedAt
	}
	status := hardcover.StatusCurrentlyReading
	var finishedAt *time.Time
	if book.ABSIsFinished {
		status = hardcover.StatusRead
		if book.ABSFinishedAt != nil {
			finishedAt = book.ABSFinishedAt
		} else {
			now := time.Now()
			finishedAt = &now
		}
	}
	// Record progress in the unit the matched edition uses: audiobook editions
	// track seconds, but if a book only has a physical/ebook edition on Hardcover
	// (no audiobook), Hardcover shows seconds as 0% — it needs pages there. So map
	// the ABS listening fraction onto the edition's page count for those.
	progress, err := s.progressForEdition(ctx, book)
	if err != nil {
		return fmt.Errorf("resolve edition progress: %w", err)
	}

	// A read has to hang off a user_book, so make sure one exists first.
	ub, err := s.hc.GetUserBook(ctx, *book.HCBookID)
	if err != nil {
		return fmt.Errorf("get user_book: %w", err)
	}
	var userBookID int64
	var lastReadID *int64
	if ub == nil {
		userBookID, err = s.hc.InsertUserBook(ctx, *book.HCBookID, *book.HCEditionID, status)
		if err != nil {
			return fmt.Errorf("insert user_book: %w", err)
		}
	} else {
		userBookID = ub.ID
		lastReadID = ub.ReadID
	}

	if reread || lastReadID == nil {
		if _, err := s.hc.InsertUserBookRead(ctx, userBookID, *book.HCEditionID, startedAt, progress, finishedAt); err != nil {
			return fmt.Errorf("insert read: %w", err)
		}
	} else {
		if err := s.hc.UpdateUserBookRead(ctx, *lastReadID, startedAt, progress, finishedAt); err != nil {
			return fmt.Errorf("update read: %w", err)
		}
	}

	if err := s.hc.UpdateUserBookStatus(ctx, userBookID, status); err != nil {
		s.log.Warn("update HC status", "book_id", bookID, "err", err)
	}

	// Reflect what we just pushed locally so the card stops flagging drift. Drift
	// is tracked in ABS seconds regardless of the unit pushed to Hardcover.
	// Pushing real progress means the book is being read again, not DNF.
	if err := s.db.UpdateHCProgress(ctx, bookID, book.ABSCurrentSeconds, book.ABSIsFinished, false); err != nil {
		s.log.Warn("update local HC progress", "book_id", bookID, "err", err)
	}
	s.log.Info("pushed progress to HC",
		"book_id", bookID, "reread", reread, "progress", progress, "finished", book.ABSIsFinished)
	return nil
}

// AutoSyncOutOfSync pushes ABS progress to Hardcover for every matched book whose
// progress has drifted (the "Progress out of sync" category), skipping books
// marked Did-Not-Finish. It returns how many books were synced. A single book's
// failure is logged and skipped rather than aborting the whole pass.
func (s *Service) AutoSyncOutOfSync(ctx context.Context) (int, error) {
	matched, err := s.db.ListMatchedBooks(ctx)
	if err != nil {
		return 0, fmt.Errorf("list matched: %w", err)
	}
	synced := 0
	for _, b := range matched {
		if b.HCDNF || !b.ProgressDiffers() {
			continue
		}
		if err := s.PushProgress(ctx, b.ID, false); err != nil {
			s.log.Warn("auto-sync push", "book_id", b.ID, "title", b.ABSTitle, "err", err)
			continue
		}
		synced++
	}
	if synced > 0 {
		s.log.Info("auto-synced out-of-sync books", "count", synced)
	}
	return synced, nil
}

// progressForEdition maps the book's current ABS progress onto the unit the
// matched Hardcover edition uses. Audiobook editions take seconds; editions with
// no audio length but a page count (physical/ebook) take a page count scaled by
// how far through the ABS audiobook the listener is.
func (s *Service) progressForEdition(ctx context.Context, book db.Book) (hardcover.ReadProgress, error) {
	seconds := hardcover.ReadProgress{Seconds: int(math.Round(book.ABSCurrentSeconds))}
	if book.HCEditionID == nil {
		return seconds, nil
	}
	ed, err := s.hc.GetEdition(ctx, *book.HCEditionID)
	if err != nil {
		return hardcover.ReadProgress{}, err
	}
	// Audiobook (or any edition that reports an audio length): use seconds.
	if ed.AudioSeconds > 0 || ed.ReadingFormatID == hardcover.FormatAudiobook {
		return seconds, nil
	}
	// Page-based edition: scale the ABS listening fraction onto its pages.
	if ed.Pages > 0 && book.ABSTotalSeconds > 0 {
		frac := book.ABSCurrentSeconds / book.ABSTotalSeconds
		if frac > 1 {
			frac = 1
		}
		pages := int(math.Round(frac * float64(ed.Pages)))
		if pages < 1 && book.ABSCurrentSeconds > 0 {
			pages = 1 // some progress shouldn't round down to "not started"
		}
		return hardcover.ReadProgress{Pages: pages}, nil
	}
	// No usable page count — fall back to seconds (better than nothing).
	return seconds, nil
}

// MarkDNF flags a matched book as "Did Not Finish" on Hardcover, setting the
// user_book status accordingly (creating the user_book first if needed) and
// recording the DNF state locally so the UI can surface it in its own category.
func (s *Service) MarkDNF(ctx context.Context, bookID int64) error {
	book, err := s.db.GetBook(ctx, bookID)
	if err != nil {
		return fmt.Errorf("get book: %w", err)
	}
	if book.HCBookID == nil || book.HCEditionID == nil {
		return fmt.Errorf("book is not matched to a Hardcover edition")
	}

	// A status has to hang off a user_book, so make sure one exists first.
	ub, err := s.hc.GetUserBook(ctx, *book.HCBookID)
	if err != nil {
		return fmt.Errorf("get user_book: %w", err)
	}
	var userBookID int64
	if ub == nil {
		userBookID, err = s.hc.InsertUserBook(ctx, *book.HCBookID, *book.HCEditionID, hardcover.StatusDidNotFinish)
		if err != nil {
			return fmt.Errorf("insert user_book: %w", err)
		}
	} else {
		userBookID = ub.ID
	}

	if err := s.hc.UpdateUserBookStatus(ctx, userBookID, hardcover.StatusDidNotFinish); err != nil {
		return fmt.Errorf("set DNF status: %w", err)
	}

	// Keep the recorded progress, but flag the book as DNF locally.
	if err := s.db.UpdateHCProgress(ctx, bookID, book.ABSCurrentSeconds, false, true); err != nil {
		s.log.Warn("update local DNF state", "book_id", bookID, "err", err)
	}
	s.log.Info("marked DNF on HC", "book_id", bookID)
	return nil
}

// libraryIndex indexes the user's existing Hardcover library for fast lookup
// while matching ABS books.
type libraryIndex struct {
	byISBN  map[string]hardcover.UserBookSummary
	byASIN  map[string]hardcover.UserBookSummary
	byTitle map[string]hardcover.UserBookSummary
}

func (s *Service) loadLibraryIndex(ctx context.Context) (*libraryIndex, error) {
	books, err := s.hc.GetMyUserBooks(ctx)
	if err != nil {
		return nil, err
	}
	idx := &libraryIndex{
		byISBN:  map[string]hardcover.UserBookSummary{},
		byASIN:  map[string]hardcover.UserBookSummary{},
		byTitle: map[string]hardcover.UserBookSummary{},
	}
	for _, ub := range books {
		if ub.Edition != nil {
			if ub.Edition.ISBN13 != "" {
				idx.byISBN[ub.Edition.ISBN13] = ub
			}
			if ub.Edition.ISBN10 != "" {
				idx.byISBN[ub.Edition.ISBN10] = ub
			}
			if ub.Edition.ASIN != "" {
				idx.byASIN[ub.Edition.ASIN] = ub
			}
		}
		if t := normalizeTitle(ub.Title()); t != "" {
			if _, exists := idx.byTitle[t]; !exists {
				idx.byTitle[t] = ub
			}
		}
	}
	return idx, nil
}

// matchInLibrary returns the edition to match a book to if it already exists in
// the user's Hardcover library, or nil if not found.
func (s *Service) matchInLibrary(ctx context.Context, book db.Book, idx *libraryIndex) *db.CandidateEdition {
	var ub *hardcover.UserBookSummary
	matchedBy := ""

	if book.ABSISBN != "" {
		if m, ok := idx.byISBN[book.ABSISBN]; ok {
			ub = &m
			matchedBy = "isbn (library)"
		}
	}
	if ub == nil && book.ABSASIN != "" {
		if m, ok := idx.byASIN[book.ABSASIN]; ok {
			ub = &m
			matchedBy = "asin (library)"
		}
	}
	if ub == nil {
		if t := normalizeTitle(book.ABSTitle); t != "" {
			if m, ok := idx.byTitle[t]; ok && authorsPlausible(book, m) {
				ub = &m
				matchedBy = "title+author (library)"
			}
		}
	}
	if ub == nil {
		return nil
	}

	// Prefer an audiobook edition of the matched book — ABS is audiobook-centric,
	// and the user's shelf edition is often the print/ebook. Fall back to whatever
	// edition they actually have if no audiobook edition exists.
	if eds, err := s.hc.GetEditionsByBookID(ctx, ub.BookID); err == nil && len(eds) > 0 {
		c := editionToCandidate(eds[0], matchedBy)
		return &c
	}
	if ub.Edition != nil {
		c := editionToCandidate(*ub.Edition, matchedBy)
		return &c
	}
	return nil
}

func (s *Service) searchHC(ctx context.Context, book db.Book) ([]db.CandidateEdition, error) {
	seen := map[int64]bool{}
	var result []db.CandidateEdition

	// matchedBy records how each candidate was found (isbn / asin / title+author);
	// the first search to surface an edition wins, matching the search order.
	add := func(editions []hardcover.Edition, matchedBy string) {
		for _, e := range editions {
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			result = append(result, editionToCandidate(e, matchedBy))
		}
	}

	if book.ABSISBN != "" {
		eds, err := s.hc.SearchByISBN(ctx, book.ABSISBN)
		if err != nil {
			s.log.Warn("HC ISBN search", "isbn", book.ABSISBN, "err", err)
		} else {
			add(eds, "isbn")
		}
	}

	if book.ABSASIN != "" {
		eds, err := s.hc.SearchByASIN(ctx, book.ABSASIN)
		if err != nil {
			s.log.Warn("HC ASIN search", "asin", book.ABSASIN, "err", err)
		} else {
			add(eds, "asin")
		}
	}

	// Fall back to title+author only when ISBN/ASIN turned up nothing.
	if len(result) == 0 && book.ABSTitle != "" {
		eds, err := s.hc.SearchByTitleAuthor(ctx, book.ABSTitle, book.ABSAuthor)
		if err != nil {
			s.log.Warn("HC title search", "title", book.ABSTitle, "err", err)
		} else {
			add(eds, "title+author")
		}
	}

	return result, nil
}

func editionToCandidate(e hardcover.Edition, matchedBy string) db.CandidateEdition {
	return db.CandidateEdition{
		ID:        e.ID,
		BookID:    e.BookID,
		Title:     e.DisplayTitle(),
		Author:    e.AuthorName(),
		Publisher: e.PublisherName(),
		Year:      e.ReleaseYear,
		FormatID:  e.ReadingFormatID,
		ImageURL:  e.ImageURL(),
		ISBN13:    e.ISBN13,
		ASIN:      e.ASIN,
		Slug:      e.BookSlug(),
		Readers:   e.Readers(),
		MatchedBy: matchedBy,
	}
}

// authorsPlausible guards a title-only library match: when both sides name an
// author, require they share at least one word; otherwise accept the title hit.
func authorsPlausible(book db.Book, ub hardcover.UserBookSummary) bool {
	abs := normalizeTitle(book.ABSAuthor)
	var hc string
	if ub.Book != nil {
		hc = normalizeTitle(ub.Book.AuthorName())
	}
	if hc == "" && ub.Edition != nil {
		hc = normalizeTitle(ub.Edition.AuthorName())
	}
	if abs == "" || hc == "" {
		return true
	}
	words := map[string]bool{}
	for _, w := range strings.Fields(abs) {
		words[w] = true
	}
	for _, w := range strings.Fields(hc) {
		if words[w] {
			return true
		}
	}
	return false
}

// normalizeTitle lowercases and strips punctuation/extra whitespace so titles
// and authors compare reliably across ABS and Hardcover.
func normalizeTitle(s string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			prevSpace = false
		case unicode.IsSpace(r):
			if !prevSpace && b.Len() > 0 {
				b.WriteByte(' ')
				prevSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}
