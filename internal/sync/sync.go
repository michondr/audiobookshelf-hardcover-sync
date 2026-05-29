package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/michondr/audiobookshelf-hardcover-sync/internal/abs"
	"github.com/michondr/audiobookshelf-hardcover-sync/internal/db"
	"github.com/michondr/audiobookshelf-hardcover-sync/internal/hardcover"
)

const (
	rereadThreshold      = 0.10 // drop below 10% …
	highProgressThreshold = 0.85 // … after being above 85%
)

type Service struct {
	db  *db.DB
	abs *abs.Client
	hc  *hardcover.Client
	log *slog.Logger
}

func New(database *db.DB, absClient *abs.Client, hcClient *hardcover.Client, log *slog.Logger) *Service {
	return &Service{db: database, abs: absClient, hc: hcClient, log: log}
}

// RefreshFromABS fetches all books from ABS, upserts them into the DB,
// updates progress, runs auto-matching for new unmatched books, and checks
// for potential re-reads.
func (s *Service) RefreshFromABS(ctx context.Context) error {
	books, err := s.abs.GetAllBooks(ctx)
	if err != nil {
		return fmt.Errorf("refresh from ABS: %w", err)
	}

	for _, b := range books {
		if err := s.db.UpsertABSBook(ctx, b.ItemID, b.Title, b.Author, b.ISBN, b.ASIN, b.TotalSeconds); err != nil {
			s.log.Error("upsert book", "item_id", b.ItemID, "err", err)
			continue
		}

		lastSeen := b.LastUpdate
		if lastSeen.IsZero() {
			lastSeen = time.Now()
		}
		if err := s.db.UpdateABSProgress(ctx, b.ItemID, b.CurrentSeconds, b.IsFinished, lastSeen); err != nil {
			s.log.Error("update progress", "item_id", b.ItemID, "err", err)
		}
	}

	all, err := s.db.ListAllBooks(ctx)
	if err != nil {
		return err
	}

	for _, book := range all {
		if book.Status == db.StatusUnmatched && book.CandidateEditionID == nil {
			if err := s.autoMatch(ctx, book); err != nil {
				s.log.Warn("auto-match failed", "book_id", book.ID, "title", book.ABSTitle, "err", err)
			}
		}
		if book.Status == db.StatusMatched && !book.PendingReread {
			s.checkReread(ctx, book)
		}
	}

	return nil
}

// checkReread sets the pending_reread flag when ABS progress has reset near
// the start after previously being near the end. Requires total_seconds to
// be populated; skips silently if not available.
func (s *Service) checkReread(ctx context.Context, b db.Book) {
	if b.ABSTotalSeconds <= 0 || b.LastSyncedSeconds <= 0 {
		return
	}
	lastPct := b.LastSyncedSeconds / b.ABSTotalSeconds
	currPct := b.ABSCurrentSeconds / b.ABSTotalSeconds
	if lastPct >= highProgressThreshold && currPct <= rereadThreshold {
		if err := s.db.SetPendingReread(ctx, b.ID, true); err != nil {
			s.log.Error("set pending reread", "book_id", b.ID, "err", err)
		}
	}
}

// autoMatch tries ISBN → ASIN → title+author and stores the best candidate.
func (s *Service) autoMatch(ctx context.Context, book db.Book) error {
	var editions []hardcover.Edition
	var reason string

	if book.ABSISBN != "" {
		if e, err := s.hc.SearchByISBN(ctx, book.ABSISBN); err == nil && len(e) > 0 {
			editions, reason = e, "isbn"
		}
	}
	if len(editions) == 0 && book.ABSASIN != "" {
		if e, err := s.hc.SearchByASIN(ctx, book.ABSASIN); err == nil && len(e) > 0 {
			editions, reason = e, "asin"
		}
	}
	if len(editions) == 0 && book.ABSTitle != "" {
		if e, err := s.hc.SearchByTitleAuthor(ctx, book.ABSTitle, book.ABSAuthor); err == nil && len(e) > 0 {
			editions, reason = e, "title_author"
		}
	}

	if len(editions) == 0 {
		return nil
	}

	e := editions[0]
	return s.db.SetMatchCandidate(ctx, book.ID, db.MatchCandidate{
		EditionID:  e.ID,
		BookID:     e.BookID,
		Title:      e.DisplayTitle(),
		Author:     e.AuthorName(),
		Publisher:  e.PublisherName(),
		Year:       e.YearStr(),
		ImageURL:   e.ImageURL(),
		Reason:     reason,
	})
}

// ConfirmMatch confirms a match (auto-detected or manually entered), adds the
// book to the user's Hardcover library, and creates/updates the reading session.
func (s *Service) ConfirmMatch(ctx context.Context, bookID int64, editionID int64) error {
	book, err := s.db.GetBook(ctx, bookID)
	if err != nil {
		return fmt.Errorf("get book: %w", err)
	}

	edition, err := s.hc.GetEdition(ctx, editionID)
	if err != nil {
		return fmt.Errorf("get edition %d: %w", editionID, err)
	}

	statusID := hardcover.StatusCurrentlyReading
	if book.ABSIsFinished {
		statusID = hardcover.StatusRead
	} else if book.ABSCurrentSeconds == 0 {
		statusID = hardcover.StatusWantToRead
	}

	// Reuse existing user_book if the user already has this book in their HC library.
	existing, err := s.hc.GetUserBook(ctx, edition.BookID)
	if err != nil {
		return fmt.Errorf("check existing user book: %w", err)
	}

	var userBookID int64
	var userBookReadID int64

	if existing != nil {
		userBookID = existing.ID
		if existing.ReadID != nil {
			userBookReadID = *existing.ReadID
		}
	} else {
		userBookID, err = s.hc.InsertUserBook(ctx, edition.BookID, editionID, statusID)
		if err != nil {
			return fmt.Errorf("insert user book: %w", err)
		}
	}

	// Create or update the reading session.
	now := time.Now()
	var finishedAt *time.Time
	if book.ABSIsFinished {
		finishedAt = &now
	}

	if book.ABSCurrentSeconds > 0 {
		if userBookReadID == 0 {
			readID, readErr := s.hc.InsertUserBookRead(ctx, userBookID, editionID, now, book.ABSCurrentSeconds, finishedAt)
			if readErr != nil {
				s.log.Warn("insert user book read failed — book added to HC but no reading session",
					"book_id", bookID, "hc_user_book_id", userBookID, "err", readErr)
			}
			userBookReadID = readID
		} else {
			_ = s.hc.UpdateUserBookRead(ctx, userBookReadID, book.ABSCurrentSeconds, finishedAt)
		}
	}

	if existing == nil {
		_ = s.hc.UpdateUserBookStatus(ctx, userBookID, statusID)
	}

	return s.db.SetMatch(ctx, bookID, db.HCMatch{
		EditionID:      editionID,
		BookID:         edition.BookID,
		UserBookID:     userBookID,
		UserBookReadID: userBookReadID,
		EditionTitle:   edition.DisplayTitle(),
		Publisher:      edition.PublisherName(),
		Year:           edition.YearStr(),
		Format:         edition.FormatName(),
		ImageURL:       edition.ImageURL(),
	})
}

// SyncBookProgress pushes current ABS progress for a single matched book to Hardcover.
func (s *Service) SyncBookProgress(ctx context.Context, bookID int64) error {
	book, err := s.db.GetBook(ctx, bookID)
	if err != nil {
		return fmt.Errorf("get book: %w", err)
	}
	if book.Status != db.StatusMatched {
		return fmt.Errorf("book %d is not matched", bookID)
	}
	if book.HCUserBookID == nil {
		return fmt.Errorf("book %d has no HC user_book_id", bookID)
	}

	// Create a reading session if one doesn't exist yet.
	if (book.HCUserBookReadID == nil || *book.HCUserBookReadID == 0) && book.HCEditionID != nil && book.ABSCurrentSeconds > 0 {
		now := time.Now()
		var finishedAt *time.Time
		if book.ABSIsFinished {
			finishedAt = &now
		}
		readID, err := s.hc.InsertUserBookRead(ctx, *book.HCUserBookID, *book.HCEditionID, now, book.ABSCurrentSeconds, finishedAt)
		if err != nil {
			return fmt.Errorf("create reading session: %w", err)
		}
		if err := s.db.SetHCUserBookReadID(ctx, bookID, readID); err != nil {
			return err
		}
		book.HCUserBookReadID = &readID
	}

	if book.HCUserBookReadID == nil || *book.HCUserBookReadID == 0 {
		return fmt.Errorf("no reading session available for book %d", bookID)
	}

	now := time.Now()
	var finishedAt *time.Time
	if book.ABSIsFinished {
		finishedAt = &now
	}

	if err := s.hc.UpdateUserBookRead(ctx, *book.HCUserBookReadID, book.ABSCurrentSeconds, finishedAt); err != nil {
		return fmt.Errorf("update progress: %w", err)
	}

	statusID := hardcover.StatusCurrentlyReading
	if book.ABSIsFinished {
		statusID = hardcover.StatusRead
	}
	_ = s.hc.UpdateUserBookStatus(ctx, *book.HCUserBookID, statusID)

	return s.db.UpdateLastSynced(ctx, bookID, book.ABSCurrentSeconds, book.ABSIsFinished, now)
}

// SyncAllProgress syncs all matched books that need a progress update.
// Returns the count of books successfully synced.
func (s *Service) SyncAllProgress(ctx context.Context) (int, error) {
	cats, err := s.db.ListBooksByCategory(ctx)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, book := range cats.NeedsSync {
		if book.PendingReread {
			continue // waiting for user confirmation
		}
		if err := s.SyncBookProgress(ctx, book.ID); err != nil {
			s.log.Error("sync book progress", "book_id", book.ID, "title", book.ABSTitle, "err", err)
			continue
		}
		count++
	}
	return count, nil
}

// ConfirmReread starts a new reading session in Hardcover for a re-read.
func (s *Service) ConfirmReread(ctx context.Context, bookID int64) error {
	book, err := s.db.GetBook(ctx, bookID)
	if err != nil {
		return err
	}
	if book.HCUserBookID == nil || book.HCEditionID == nil {
		return fmt.Errorf("book %d not matched", bookID)
	}

	readID, err := s.hc.InsertUserBookRead(ctx, *book.HCUserBookID, *book.HCEditionID, time.Now(), book.ABSCurrentSeconds, nil)
	if err != nil {
		return fmt.Errorf("insert re-read session: %w", err)
	}
	if err := s.hc.UpdateUserBookStatus(ctx, *book.HCUserBookID, hardcover.StatusCurrentlyReading); err != nil {
		return err
	}
	if err := s.db.SetHCUserBookReadID(ctx, bookID, readID); err != nil {
		return err
	}
	if err := s.db.SetPendingReread(ctx, bookID, false); err != nil {
		return err
	}
	return s.db.UpdateLastSynced(ctx, bookID, book.ABSCurrentSeconds, false, time.Now())
}

// DismissReread clears the re-read flag without creating a new session.
func (s *Service) DismissReread(ctx context.Context, bookID int64) error {
	book, err := s.db.GetBook(ctx, bookID)
	if err != nil {
		return err
	}
	if err := s.db.SetPendingReread(ctx, bookID, false); err != nil {
		return err
	}
	return s.db.UpdateLastSynced(ctx, bookID, book.ABSCurrentSeconds, book.ABSIsFinished, time.Now())
}
