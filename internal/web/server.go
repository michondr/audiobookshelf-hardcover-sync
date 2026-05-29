package web

import (
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/michondr/audiobookshelf-hardcover-sync/internal/abs"
	"github.com/michondr/audiobookshelf-hardcover-sync/internal/db"
	syncsvc "github.com/michondr/audiobookshelf-hardcover-sync/internal/sync"
)

//go:embed static
var staticFiles embed.FS

func NewServer(
	database *db.DB,
	absClient *abs.Client,
	syncService *syncsvc.Service,
	log *slog.Logger,
	nextSync func() time.Time,
) http.Handler {
	h := &handler{
		db:       database,
		abs:      absClient,
		sync:     syncService,
		log:      log,
		nextSync: nextSync,
	}

	mux := http.NewServeMux()

	// Static files
	sub, _ := fs.Sub(staticFiles, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))

	// Pages
	mux.HandleFunc("/", h.handleIndex)
	mux.HandleFunc("/abs-link/", h.handleAbsLink)

	// Proxy
	mux.HandleFunc("/proxy/abs-cover/", h.handleAbsCoverProxy)

	// Actions
	mux.HandleFunc("/sync", requirePOST(h.handleSyncAll))
	mux.HandleFunc("/books/", h.routeBooks)

	return mux
}

func (h *handler) routeBooks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := r.URL.Path
	switch {
	case hasSuffix(path, "/ignore"):
		h.handleBookIgnore(w, r)
	case hasSuffix(path, "/unignore"):
		h.handleBookUnignore(w, r)
	case hasSuffix(path, "/confirm-match"):
		h.handleConfirmMatch(w, r)
	case hasSuffix(path, "/manual-match"):
		h.handleManualMatch(w, r)
	case hasSuffix(path, "/sync-progress"):
		h.handleSyncProgress(w, r)
	case hasSuffix(path, "/confirm-reread"):
		h.handleConfirmReread(w, r)
	case hasSuffix(path, "/dismiss-reread"):
		h.handleDismissReread(w, r)
	default:
		http.NotFound(w, r)
	}
}

func requirePOST(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	}
}

func hasSuffix(path, suffix string) bool {
	return len(path) > len(suffix) && path[len(path)-len(suffix):] == suffix
}
