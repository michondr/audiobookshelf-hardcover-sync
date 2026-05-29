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

// rereadThreshold: if current progress drops below this after having been above highProgressThreshold
const (
	rereadThreshold     = 0.10 // 10%
	highProgressThreshold = 0.85 // 85%
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
// updates progress, and runs auto-matching for newly discovered unmatched books.
func (s *Service) RefreshFromABS(ctx context.Context) error {
	books, err := s.abs.GetAllBooks(ctx)
	if err != nil {
		return fmt.Errorf("refresh from ABS: %w", err)
	}

	for _, b := range books {
		if err := s.db.UpsertABSBook(ctx, b.ItemID, b.Title, b.Author, b.ISBN, b.ASIN); err != nil {
			s.log.Error("upsert book", "item_id", b.ItemID, "err", err)
			continue
		}

		lastSeen := b.LastUpdate
		if lastSeen.IsZero() {
			lastSeen = time.Now()
		}

		if err := s.db.UpdateABSProgress(ctx, b.ItemID, b.CurrentSeconds, b.IsFinished, lastSeen); err != nil {
			s.log.Error("update progress", "item_id", b.ItemID, "err", err)
			continue
		}
	}

	// Auto-match any unmatched books that don't yet have a candidate
	all, err := s.db.ListAllBooks(ctx)
	if err != nil {
		return err
	}
	for _, book := range all {
		if book.Status != "unmatched" {
			continue
		}
		if book.CandidateEditionID != nil {
			continue // already has a candidate
		}
		if err := s.autoMatch(ctx, book); err != nil {
			s.log.Warn("auto-match failed", "book_id", book.ID, "title", book.ABSTitle, "err", err)
		}
	}

	return nil
}

// autoMatch tries ISBN → ASIN → title+author matching and stores the best candidate.
func (s *Service) autoMatch(ctx context.Context, book db.Book) error {
	var editions []hardcover.Edition
	var reason string

	// Try ISBN
	if book.ABSISBN != "" {
		if e, err := s.hc.SearchByISBN(ctx, book.ABSISBN); err == nil && len(e) > 0 {
			editions = e
			reason = "isbn"
		}
	}

	// Try ASIN
	if len(editions) == 0 && book.ABSASIN != "" {
		if e, err := s.hc.SearchByASIN(ctx, book.ABSASIN); err == nil && len(e) > 0 {
			editions = e
			reason = "asin"
		}
	}

	// Try title + author
	if len(editions) == 0 && book.ABSTitle != "" {
		if e, err := s.hc.SearchByTitleAuthor(ctx, book.ABSTitle, book.ABSAuthor); err == nil && len(e) > 0 {
			editions = e
			reason = "title_author"
		}
	}

	if len(editions) == 0 {
		return nil // no match found, leave candidate empty
	}

	e := editions[0]
	year := ""
	if e.ReleaseYear > 0 {
		year = fmt.Sprintf("%d", e.ReleaseYear)
	}

	return s.db.SetMatchCandidate(ctx, book.ID, db.MatchCandidate{
		EditionID:  e.ID,
		BookID:     e.BookID,
		Title:      e.DisplayTitle(),
		Author:     e.AuthorName(),
		Publisher:  e.PublisherName(),
		Year:       year,
		ImageURL:   e.ImageURL(),
		Reason:     reason,
	})
}

// ConfirmMatch confirms a match (auto-detected or manually entered), adds the book
// to the user's Hardcover library, creates/picks a reading session, and updates the DB.
func (s *Service) ConfirmMatch(ctx context.Context, bookID int64, editionID int64) error {
	book, err := s.db.GetBook(ctx, bookID)
	if err != nil {
		return fmt.Errorf("get book: %w", err)
	}

	edition, err := s.hc.GetEdition(ctx, editionID)
	if err != nil {
		return fmt.Errorf("get edition %d: %w", editionID, err)
	}

	// Determine initial HC status
	statusID := hardcover.StatusCurrentlyReading
	if book.ABSIsFinished {
		statusID = hardcover.StatusRead
	} else if book.ABSCurrentSeconds == 0 {
		statusID = hardcover.StatusWantToRead
	}

	// Check if user already has this book in their HC library
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
		// Add to Hardcover library
		userBookID, err = s.hc.InsertUserBook(ctx, edition.BookID, editionID, statusID)
		if err != nil {
			return fmt.Errorf("insert user book: %w", err)
		}
	}

	// Create or update reading session
	now := time.Now()
	if userBookReadID == 0 && book.ABSCurrentSeconds > 0 {
		var finishedAt *time.Time
		if book.ABSIsFinished {
			finishedAt = &now
		}
		_ = finishedAt // used below

		var readErr error
		userBookReadID, readErr = s.hc.InsertUserBookRead(ctx, userBookID, editionID, now, book.ABSCurrentSeconds)
		if readErr != nil {
			s.log.Warn("insert user book read failed", "err", readErr)
		}
		if book.ABSIsFinished {
			_ = s.hc.UpdateUserBookRead(ctx, userBookReadID, book.ABSCurrentSeconds, &now)
		}
	} else if userBookReadID != 0 && book.ABSCurrentSeconds > 0 {
		var finishedAt *time.Time
		if book.ABSIsFinished {
			finishedAt = &now
		}
		_ = s.hc.UpdateUserBookRead(ctx, userBookReadID, book.ABSCurrentSeconds, finishedAt)
	}

	// Update HC book status if we just added it
	if existing == nil {
		_ = s.hc.UpdateUserBookStatus(ctx, userBookID, statusID)
	}

	year := ""
	if edition.ReleaseYear > 0 {
		year = fmt.Sprintf("%d", edition.ReleaseYear)
	}
	formatName := "Audiobook"
	if edition.ReadingFormatID == hardcover.FormatEbook {
		formatName = "Ebook"
	}

	return s.db.SetMatch(ctx, bookID, db.HCMatch{
		EditionID:      editionID,
		BookID:         edition.BookID,
		UserBookID:     userBookID,
		UserBookReadID: userBookReadID,
		EditionTitle:   edition.DisplayTitle(),
		Publisher:      edition.PublisherName(),
		Year:           year,
		Format:         formatName,
		ImageURL:       edition.ImageURL(),
	})
}

// SyncBookProgress pushes current ABS progress for a single matched book to Hardcover.
func (s *Service) SyncBookProgress(ctx context.Context, bookID int64) error {
	book, err := s.db.GetBook(ctx, bookID)
	if err != nil {
		return fmt.Errorf("get book: %w", err)
	}
	if book.Status != "matched" {
		return fmt.Errorf("book %d is not matched", bookID)
	}
	if book.HCUserBookID == nil {
		return fmt.Errorf("book %d has no HC user_book_id", bookID)
	}

	// Ensure we have an active reading session
	if book.HCUserBookReadID == nil || *book.HCUserBookReadID == 0 {
		if book.HCEditionID != nil && book.ABSCurrentSeconds > 0 {
			readID, err := s.hc.InsertUserBookRead(ctx, *book.HCUserBookID, *book.HCEditionID, time.Now(), book.ABSCurrentSeconds)
			if err != nil {
				return fmt.Errorf("create reading session: %w", err)
			}
			if err := s.db.SetHCUserBookReadID(ctx, bookID, readID); err != nil {
				return err
			}
			book.HCUserBookReadID = &readID
		}
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

	// Update HC status
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
			// skip — waiting for user confirmation on re-read
			continue
		}
		if err := s.SyncBookProgress(ctx, book.ID); err != nil {
			s.log.Error("sync book progress", "book_id", book.ID, "title", book.ABSTitle, "err", err)
			continue
		}
		count++
	}
	return count, nil
}

// CheckReread inspects all matched books for potential re-reads and sets the flag.
func (s *Service) CheckReread(ctx context.Context) error {
	books, err := s.db.ListAllBooks(ctx)
	if err != nil {
		return err
	}

	for _, b := range books {
		if b.Status != "matched" || b.PendingReread {
			continue
		}
		// If the book was previously synced at high progress and is now near the start
		if b.LastSyncedSeconds > 0 {
			lastPct := b.LastSyncedSeconds / max(b.ABSCurrentSeconds, b.LastSyncedSeconds)
			currPct := b.ABSCurrentSeconds / max(b.ABSCurrentSeconds, b.LastSyncedSeconds)
			if lastPct >= highProgressThreshold && currPct <= rereadThreshold {
				if err := s.db.SetPendingReread(ctx, b.ID, true); err != nil {
					s.log.Error("set pending reread", "book_id", b.ID, "err", err)
				}
			}
		}
	}
	return nil
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

	readID, err := s.hc.InsertUserBookRead(ctx, *book.HCUserBookID, *book.HCEditionID, time.Now(), book.ABSCurrentSeconds)
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
	// Treat current progress as "already synced" to avoid immediately re-triggering
	return s.db.UpdateLastSynced(ctx, bookID, book.ABSCurrentSeconds, book.ABSIsFinished, time.Now())
}

