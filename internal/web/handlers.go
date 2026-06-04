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

// handleHealthz is a liveness/readiness probe: it returns 200 only when the
// database connection is reachable, otherwise 503.
func (h *handler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := h.db.Ping(r.Context()); err != nil {
		http.Error(w, "db unreachable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, "ok")
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
	if h.sync.Matching() {
		nextSync = "⏳ Matching… · " + nextSync
	}

	autoSync, err := h.db.GetBoolSetting(r.Context(), db.SettingAutoSyncProgress)
	if err != nil {
		h.log.Warn("read auto-sync setting", "err", err)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.Index(books, nextSync, autoSync).Render(r.Context(), w)
}

// handleSetAutoSync persists the "auto sync progress to HC" toggle. The checkbox
// only sends its value when checked, so an absent value means "off".
func (h *handler) handleSetAutoSync(w http.ResponseWriter, r *http.Request) {
	enabled := r.FormValue("enabled") != ""
	if err := h.db.SetBoolSetting(r.Context(), db.SettingAutoSyncProgress, enabled); err != nil {
		h.log.Error("save auto-sync setting", "err", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if enabled {
		fmt.Fprint(w, `<div class="toast">Auto-sync enabled — the scheduled sync will push out-of-sync progress to Hardcover.</div>`)
	} else {
		fmt.Fprint(w, `<div class="toast">Auto-sync disabled.</div>`)
	}
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

	// Matching hits Hardcover for many books and can take minutes, so run it in
	// the background rather than blocking the request (and tripping the server's
	// write timeout).
	started := h.sync.MatchUnmatchedInBackground()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if started {
		fmt.Fprint(w, `<div class="toast">Synced from ABS. Matching against Hardcover in the background — refresh in a minute to see results.</div>`)
	} else {
		fmt.Fprint(w, `<div class="toast">Synced from ABS. A match pass is already running.</div>`)
	}
}

func (h *handler) handleRematch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if h.sync.Matching() {
		fmt.Fprint(w, `<div class="toast">A match pass is already running.</div>`)
		return
	}

	if err := h.db.ResetUnmatchedSearch(r.Context()); err != nil {
		h.log.Error("reset unmatched search", "err", err)
		fmt.Fprintf(w, `<div class="toast error">Re-match error: %s</div>`, err.Error())
		return
	}

	h.sync.MatchUnmatchedInBackground()
	fmt.Fprint(w, `<div class="toast">Re-matching every un-matched book against Hardcover in the background — refresh in a minute to see results.</div>`)
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
		Slug:      edition.BookSlug(),
		Readers:   edition.Readers(),
		MatchedBy: "manual",
	}

	// When the user picked one of the suggested candidates (rather than typing an
	// edition ID), keep how that candidate was originally found.
	if book, err := h.db.GetBook(r.Context(), id); err == nil {
		for _, c := range book.ParsedCandidates() {
			if c.ID == editionID && c.MatchedBy != "" {
				candidate.MatchedBy = c.MatchedBy + " (picked)"
				break
			}
		}
	}

	if err := h.db.SetHCEdition(r.Context(), id, candidate); err != nil {
		h.log.Error("set edition in db", "id", id, "err", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	h.renderBookCard(w, r, id)
}

// handleSyncProgress updates the book's most recent Hardcover read with the
// current ABS progress. handleAddReread always starts a fresh read instead.
// Both re-render the card; on error they return a non-2xx so HTMX leaves the
// existing card untouched.
func (h *handler) handleSyncProgress(w http.ResponseWriter, r *http.Request) {
	h.pushProgress(w, r, false)
}

func (h *handler) handleAddReread(w http.ResponseWriter, r *http.Request) {
	h.pushProgress(w, r, true)
}

func (h *handler) pushProgress(w http.ResponseWriter, r *http.Request, reread bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.sync.PushProgress(r.Context(), id, reread); err != nil {
		h.log.Error("push progress to HC", "id", id, "reread", reread, "err", err)
		http.Error(w, "Hardcover update failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	h.renderBookCard(w, r, id)
}

// handleMarkDNF flags a still-in-progress book as "Did Not Finish" on Hardcover.
func (h *handler) handleMarkDNF(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.sync.MarkDNF(r.Context(), id); err != nil {
		h.log.Error("mark DNF on HC", "id", id, "err", err)
		http.Error(w, "Hardcover update failed: "+err.Error(), http.StatusBadGateway)
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
