package web

import (
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/michondr/audiobookshelf-hardcover-sync/internal/abs"
	"github.com/michondr/audiobookshelf-hardcover-sync/internal/db"
	"github.com/michondr/audiobookshelf-hardcover-sync/internal/hardcover"
	syncsvc "github.com/michondr/audiobookshelf-hardcover-sync/internal/sync"
)

//go:embed static
var staticFiles embed.FS

func NewServer(
	database *db.DB,
	absClient *abs.Client,
	hcClient *hardcover.Client,
	syncService *syncsvc.Service,
	log *slog.Logger,
	nextSync func() time.Time,
) http.Handler {
	h := &handler{
		db:       database,
		abs:      absClient,
		hc:       hcClient,
		sync:     syncService,
		log:      log,
		nextSync: nextSync,
	}

	mux := http.NewServeMux()

	sub, _ := fs.Sub(staticFiles, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))

	mux.HandleFunc("GET /healthz", h.handleHealthz)
	mux.HandleFunc("/", h.handleIndex)
	mux.HandleFunc("/abs-link/", h.handleAbsLink)
	mux.HandleFunc("/proxy/abs-cover/", h.handleAbsCoverProxy)
	mux.HandleFunc("POST /sync", h.handleSyncAll)
	mux.HandleFunc("POST /rematch", h.handleRematch)
	mux.HandleFunc("POST /settings/auto-sync", h.handleSetAutoSync)
	mux.HandleFunc("POST /book/{id}/set-edition", h.handleSetEdition)
	mux.HandleFunc("POST /book/{id}/sync-progress", h.handleSyncProgress)
	mux.HandleFunc("POST /book/{id}/reread", h.handleAddReread)
	mux.HandleFunc("POST /book/{id}/dnf", h.handleMarkDNF)
	mux.HandleFunc("POST /book/{id}/unmatch", h.handleUnmatchBook)
	mux.HandleFunc("POST /book/{id}/ignore", h.handleIgnoreBook)
	mux.HandleFunc("POST /book/{id}/unignore", h.handleUnignoreBook)

	return mux
}
