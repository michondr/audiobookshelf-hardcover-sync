package sync

import (
	"context"
	"fmt"
	"log/slog"
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

		lastSeen := b.LastUpdate
		if lastSeen.IsZero() {
			lastSeen = time.Now()
		}
		if err := s.db.UpdateABSProgress(ctx, b.ItemID, b.CurrentSeconds, b.IsFinished, lastSeen, b.StartedAt, b.FinishedAt); err != nil {
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

	if book.ABSISBN != "" {
		if m, ok := idx.byISBN[book.ABSISBN]; ok {
			ub = &m
		}
	}
	if ub == nil && book.ABSASIN != "" {
		if m, ok := idx.byASIN[book.ABSASIN]; ok {
			ub = &m
		}
	}
	if ub == nil {
		if t := normalizeTitle(book.ABSTitle); t != "" {
			if m, ok := idx.byTitle[t]; ok && authorsPlausible(book, m) {
				ub = &m
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
		c := editionToCandidate(eds[0])
		return &c
	}
	if ub.Edition != nil {
		c := editionToCandidate(*ub.Edition)
		return &c
	}
	return nil
}

func (s *Service) searchHC(ctx context.Context, book db.Book) ([]db.CandidateEdition, error) {
	seen := map[int64]bool{}
	var result []db.CandidateEdition

	add := func(editions []hardcover.Edition) {
		for _, e := range editions {
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			result = append(result, editionToCandidate(e))
		}
	}

	if book.ABSISBN != "" {
		eds, err := s.hc.SearchByISBN(ctx, book.ABSISBN)
		if err != nil {
			s.log.Warn("HC ISBN search", "isbn", book.ABSISBN, "err", err)
		} else {
			add(eds)
		}
	}

	if book.ABSASIN != "" {
		eds, err := s.hc.SearchByASIN(ctx, book.ABSASIN)
		if err != nil {
			s.log.Warn("HC ASIN search", "asin", book.ABSASIN, "err", err)
		} else {
			add(eds)
		}
	}

	// Fall back to title+author only when ISBN/ASIN turned up nothing.
	if len(result) == 0 && book.ABSTitle != "" {
		eds, err := s.hc.SearchByTitleAuthor(ctx, book.ABSTitle, book.ABSAuthor)
		if err != nil {
			s.log.Warn("HC title search", "title", book.ABSTitle, "err", err)
		} else {
			add(eds)
		}
	}

	return result, nil
}

func editionToCandidate(e hardcover.Edition) db.CandidateEdition {
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
