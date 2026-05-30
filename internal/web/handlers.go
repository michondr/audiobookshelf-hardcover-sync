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
	"github.com/michondr/audiobookshelf-hardcover-sync/internal/hardcover"
	syncsvc "github.com/michondr/audiobookshelf-hardcover-sync/internal/sync"
	"github.com/michondr/audiobookshelf-hardcover-sync/internal/web/templates"
)

type handler struct {
	db       *db.DB
	abs      *abs.Client
	hc       *hardcover.Client
	sync     *syncsvc.Service
	log      *slog.Logger
	nextSync func() time.Time
}

func (h *handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	books, err := h.db.ListAllBooks(r.Context())
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	nextSync := "—"
	if ns := h.nextSync(); !ns.IsZero() {
		nextSync = "Next sync: " + ns.Format("Mon 15:04")
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.Index(books, nextSync).Render(r.Context(), w)
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
	if err := h.sync.RefreshFromABS(r.Context()); err != nil {
		h.log.Error("refresh from ABS", "err", err)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<div class="toast error">Sync error: %s</div>`, err.Error())
		return
	}

	if err := h.sync.MatchUnmatched(r.Context()); err != nil {
		h.log.Error("match unmatched", "err", err)
	}

	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) handleSetEdition(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	editionID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("edition_id")), 10, 64)
	if err != nil || editionID <= 0 {
		http.Error(w, "invalid edition_id", http.StatusBadRequest)
		return
	}

	edition, err := h.hc.GetEdition(r.Context(), editionID)
	if err != nil {
		h.log.Warn("get edition from HC", "edition_id", editionID, "err", err)
		http.Error(w, "edition not found on Hardcover", http.StatusBadGateway)
		return
	}

	candidate := db.CandidateEdition{
		ID:        edition.ID,
		BookID:    edition.BookID,
		Title:     edition.DisplayTitle(),
		Author:    edition.AuthorName(),
		Publisher: edition.PublisherName(),
		Year:      edition.ReleaseYear,
		FormatID:  edition.ReadingFormatID,
		ImageURL:  edition.ImageURL(),
		ISBN13:    edition.ISBN13,
		ASIN:      edition.ASIN,
	}

	if err := h.db.SetHCEdition(r.Context(), id, candidate); err != nil {
		h.log.Error("set edition in db", "id", id, "err", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	h.renderBookCard(w, r, id)
}

func (h *handler) handleIgnoreBook(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.db.SetHCIgnored(r.Context(), id, true); err != nil {
		h.log.Error("ignore book", "id", id, "err", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	h.renderBookCard(w, r, id)
}

func (h *handler) handleUnmatchBook(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.db.UnmatchBook(r.Context(), id); err != nil {
		h.log.Error("unmatch book", "id", id, "err", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	h.renderBookCard(w, r, id)
}

func (h *handler) handleUnignoreBook(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.db.UnignoreBook(r.Context(), id); err != nil {
		h.log.Error("unignore book", "id", id, "err", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	h.renderBookCard(w, r, id)
}

func (h *handler) renderBookCard(w http.ResponseWriter, r *http.Request, id int64) {
	book, err := h.db.GetBook(r.Context(), id)
	if err != nil {
		http.Error(w, "book not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.BookCard(book).Render(r.Context(), w)
}
