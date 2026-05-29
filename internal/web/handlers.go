package web

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/michondr/audiobookshelf-hardcover-sync/internal/abs"
	"github.com/michondr/audiobookshelf-hardcover-sync/internal/db"
	syncsvc "github.com/michondr/audiobookshelf-hardcover-sync/internal/sync"
	"github.com/michondr/audiobookshelf-hardcover-sync/internal/web/templates"
)

type handler struct {
	db       *db.DB
	abs      *abs.Client
	sync     *syncsvc.Service
	log      *slog.Logger
	nextSync func() time.Time
}

func (h *handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()
	cats, err := h.db.ListBooksByCategory(ctx)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	nextSync := "—"
	if ns := h.nextSync(); !ns.IsZero() {
		nextSync = "Next sync: " + ns.Format("Mon 15:04")
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.Index(cats, nextSync).Render(ctx, w)
}

func (h *handler) handleAbsCoverProxy(w http.ResponseWriter, r *http.Request) {
	itemID := strings.TrimPrefix(r.URL.Path, "/proxy/abs-cover/")
	if itemID == "" {
		http.NotFound(w, r)
		return
	}

	data, ct, err := h.abs.ProxyCover(r.Context(), itemID)
	if err != nil {
		h.log.Warn("cover proxy failed", "item_id", itemID, "err", err)
		http.Error(w, "cover not available", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(data)
}

func (h *handler) handleAbsLink(w http.ResponseWriter, r *http.Request) {
	itemID := strings.TrimPrefix(r.URL.Path, "/abs-link/")
	http.Redirect(w, r, h.abs.BaseURL()+"/item/"+itemID, http.StatusFound)
}

func (h *handler) handleSyncAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	// RefreshFromABS also runs checkReread internally.
	if err := h.sync.RefreshFromABS(ctx); err != nil {
		h.log.Error("refresh before sync", "err", err)
	}
	if _, err := h.sync.SyncAllProgress(ctx); err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<div class="toast error">Sync error: %s</div>`, err.Error())
		return
	}

	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) handleBookIgnore(w http.ResponseWriter, r *http.Request) {
	book, err := h.bookFromPath(r, "/books/", "/ignore")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.db.SetStatus(r.Context(), book.ID, db.StatusIgnored); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	h.renderBook(w, r, book.ID)
}

func (h *handler) handleBookUnignore(w http.ResponseWriter, r *http.Request) {
	book, err := h.bookFromPath(r, "/books/", "/unignore")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	status := db.StatusUnmatched
	if book.HCEditionID != nil {
		status = db.StatusMatched
	}
	if err := h.db.SetStatus(r.Context(), book.ID, status); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	h.renderBook(w, r, book.ID)
}

func (h *handler) handleConfirmMatch(w http.ResponseWriter, r *http.Request) {
	book, err := h.bookFromPath(r, "/books/", "/confirm-match")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	editionIDStr := r.FormValue("edition_id")
	if editionIDStr == "" && book.CandidateEditionID != nil {
		editionIDStr = strconv.FormatInt(*book.CandidateEditionID, 10)
	}
	editionID, err := strconv.ParseInt(editionIDStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid edition_id", http.StatusBadRequest)
		return
	}

	if err := h.sync.ConfirmMatch(r.Context(), book.ID, editionID); err != nil {
		h.log.Error("confirm match", "book_id", book.ID, "edition_id", editionID, "err", err)
		http.Error(w, "match failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.renderBook(w, r, book.ID)
}

func (h *handler) handleManualMatch(w http.ResponseWriter, r *http.Request) {
	book, err := h.bookFromPath(r, "/books/", "/manual-match")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	editionID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("edition_id")), 10, 64)
	if err != nil {
		http.Error(w, "invalid edition_id", http.StatusBadRequest)
		return
	}

	if err := h.sync.ConfirmMatch(r.Context(), book.ID, editionID); err != nil {
		h.log.Error("manual match", "book_id", book.ID, "edition_id", editionID, "err", err)
		http.Error(w, "match failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.renderBook(w, r, book.ID)
}

func (h *handler) handleSyncProgress(w http.ResponseWriter, r *http.Request) {
	book, err := h.bookFromPath(r, "/books/", "/sync-progress")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := h.sync.SyncBookProgress(r.Context(), book.ID); err != nil {
		h.log.Error("sync progress", "book_id", book.ID, "err", err)
		http.Error(w, "sync failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.renderBook(w, r, book.ID)
}

func (h *handler) handleConfirmReread(w http.ResponseWriter, r *http.Request) {
	book, err := h.bookFromPath(r, "/books/", "/confirm-reread")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := h.sync.ConfirmReread(r.Context(), book.ID); err != nil {
		h.log.Error("confirm reread", "book_id", book.ID, "err", err)
		http.Error(w, "reread failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.renderBook(w, r, book.ID)
}

func (h *handler) handleDismissReread(w http.ResponseWriter, r *http.Request) {
	book, err := h.bookFromPath(r, "/books/", "/dismiss-reread")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := h.sync.DismissReread(r.Context(), book.ID); err != nil {
		h.log.Error("dismiss reread", "book_id", book.ID, "err", err)
		http.Error(w, "dismiss failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.renderBook(w, r, book.ID)
}

// ── helpers ────────────────────────────────────────────────────────────────

func (h *handler) bookFromPath(r *http.Request, prefix, suffix string) (db.Book, error) {
	idStr := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), suffix)
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return db.Book{}, fmt.Errorf("invalid book id in path")
	}
	return h.db.GetBook(r.Context(), id)
}

func (h *handler) renderBook(w http.ResponseWriter, r *http.Request, bookID int64) {
	book, err := h.db.GetBook(r.Context(), bookID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.BookCard(book).Render(r.Context(), w)
}
