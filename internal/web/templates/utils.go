package templates

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/michondr/audiobookshelf-hardcover-sync/internal/db"
)

// Progress-comparison tolerance. The matched Hardcover edition is often a
// slightly different recording than the ABS file, so the same listening spot
// maps to second offsets that diverge proportionally to how far in you are — a
// flat threshold flags long books that are really in the same place. So the
// tolerance scales with book length, with a floor to keep short books sane.
const (
	progressToleranceFloor    = 120.0 // 2 minutes
	progressToleranceFraction = 0.01  // or 1% of the audiobook's length, whichever is larger
)

// progressTolerance returns how far (in seconds) ABS and Hardcover progress may
// drift for a given book before we consider it out of sync.
func progressTolerance(b db.Book) float64 {
	if t := b.ABSTotalSeconds * progressToleranceFraction; t > progressToleranceFloor {
		return t
	}
	return progressToleranceFloor
}

// BookGroups splits a flat book list into the display categories.
type BookGroups struct {
	ProgressDiffers []db.Book // matched, but ABS progress has drifted from Hardcover
	NeedsAction     []db.Book // searched but no auto-match: candidates to pick or manual input needed
	Matched         []db.Book // confirmed edition, progress in sync, still in progress
	DNF             []db.Book // marked "did not finish" on Hardcover
	Finished        []db.Book // matched and finished on both ABS and Hardcover
	Pending         []db.Book // not yet searched
	Ignored         []db.Book
}

func groupBooks(books []db.Book) BookGroups {
	var g BookGroups
	for _, b := range books {
		switch {
		case b.HCIgnored:
			g.Ignored = append(g.Ignored, b)
		case b.HCEditionID != nil:
			switch {
			case b.HCDNF:
				g.DNF = append(g.DNF, b)
			case progressDiffers(b):
				g.ProgressDiffers = append(g.ProgressDiffers, b)
			case b.ABSIsFinished && b.HCIsFinished:
				g.Finished = append(g.Finished, b)
			default:
				g.Matched = append(g.Matched, b)
			}
		case b.HCMatchSearchedAt != nil:
			g.NeedsAction = append(g.NeedsAction, b)
		default:
			g.Pending = append(g.Pending, b)
		}
	}

	// Surface the most recently listened-to books first in the out-of-sync list —
	// they're the ones whose ABS progress most likely just moved (nulls last).
	sort.SliceStable(g.ProgressDiffers, func(i, j int) bool {
		return lastSeenAfter(g.ProgressDiffers[i].ABSLastSeenAt, g.ProgressDiffers[j].ABSLastSeenAt)
	})

	// Order matched/finished books by when reading started, falling back to when
	// the book was added to ABS when there's no start date — most recent first.
	sortByStarted := func(s []db.Book) {
		sort.SliceStable(s, func(i, j int) bool {
			return lastSeenAfter(startedOrAdded(s[i]), startedOrAdded(s[j]))
		})
	}
	sortByStarted(g.Matched)
	sortByStarted(g.Finished)

	return g
}

// startedOrAdded returns the date used to order matched books: the ABS start
// date, or the date the book was added to ABS when reading never started.
func startedOrAdded(b db.Book) *time.Time {
	if b.ABSStartedAt != nil {
		return b.ABSStartedAt
	}
	return b.ABSAddedAt
}

// lastSeenAfter reports whether a is more recent than b, treating nil as the
// oldest possible time so books without a last-seen timestamp sort to the end.
func lastSeenAfter(a, b *time.Time) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	return a.After(*b)
}

// progressDiffers reports whether a matched book's ABS progress is out of sync
// with what's recorded on Hardcover. It only judges books whose Hardcover
// progress has actually been fetched — before that we can't tell.
func progressDiffers(b db.Book) bool {
	if b.HCProgressSyncedAt == nil {
		return false
	}
	if b.ABSIsFinished != b.HCIsFinished {
		return true
	}
	if b.ABSIsFinished {
		return false
	}
	return math.Abs(b.ABSCurrentSeconds-b.HCCurrentSeconds) > progressTolerance(b)
}

// canMarkDNF reports whether the "Did not finish" action applies: the book is
// matched to a Hardcover edition, still in progress on ABS (not finished, with
// some progress), and not already flagged as DNF.
func canMarkDNF(b db.Book) bool {
	return b.HCEditionID != nil && !b.HCDNF && !b.ABSIsFinished && b.ABSCurrentSeconds > 0
}

func formatDate(t *time.Time) string {
	if t == nil {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d", t.Day(), int(t.Month()), t.Year())
}

func formatSeconds(secs float64) string {
	total := int(secs)
	h := total / 3600
	m := (total % 3600) / 60
	if h > 0 {
		return fmt.Sprintf("%dh %02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func formatRelTime(t *time.Time) string {
	if t == nil {
		return "never"
	}
	d := time.Since(*t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func hcEditionInfo(book db.Book) string {
	e := book.ParsedEdition()
	if e == nil {
		if book.HCEditionID != nil {
			return fmt.Sprintf("Edition #%d", *book.HCEditionID)
		}
		return ""
	}
	s := e.Title
	if e.Year > 0 {
		s += fmt.Sprintf(" (%d)", e.Year)
	}
	s += " · " + e.FormatName()
	return s
}

func candidateMeta(c db.CandidateEdition) string {
	parts := []string{c.FormatName()}
	if c.Year > 0 {
		parts = append(parts, fmt.Sprintf("%d", c.Year))
	}
	if c.Publisher != "" {
		parts = append(parts, c.Publisher)
	}
	return strings.Join(parts, " · ")
}

// candidateURL deep-links a candidate edition to its Hardcover page, falling
// back to the book page (then a search) when the edition slug is missing.
func candidateURL(c db.CandidateEdition) string {
	if c.Slug != "" && c.ID > 0 {
		return fmt.Sprintf("https://hardcover.app/books/%s/editions/%d", c.Slug, c.ID)
	}
	if c.Slug != "" {
		return fmt.Sprintf("https://hardcover.app/books/%s", c.Slug)
	}
	return hcSearchURL(c.Title, c.Author)
}

// candidateReaders renders the reader count, e.g. "1,234 readers".
func candidateReaders(c db.CandidateEdition) string {
	n := c.Readers
	if n == 1 {
		return "1 reader"
	}
	return fmt.Sprintf("%s readers", humanizeInt(n))
}

// humanizeInt formats an integer with thousands separators (e.g. 1234 → 1,234).
func humanizeInt(n int) string {
	s := fmt.Sprintf("%d", n)
	if n < 1000 {
		return s
	}
	var out []byte
	for i, d := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, d)
	}
	return string(out)
}

// hcBookURL deep-links to the exact matched edition on Hardcover. The slug is
// cached on the edition for books matched since slug fetching was added; older
// matches (no slug) gracefully fall back to a Hardcover search.
func hcBookURL(book db.Book) string {
	if e := book.ParsedEdition(); e != nil && e.Slug != "" && e.ID > 0 {
		return fmt.Sprintf("https://hardcover.app/books/%s/editions/%d", e.Slug, e.ID)
	}
	return hcSearchURL(book.ABSTitle, book.ABSAuthor)
}

func hcSearchURL(title, author string) string {
	q := title
	if author != "" {
		q += " " + author
	}
	return "https://hardcover.app/search?q=" + urlEncode(q)
}

func urlEncode(s string) string {
	var out []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			out = append(out, c)
		} else if c == ' ' {
			out = append(out, '+')
		} else {
			out = append(out, '%', hexChar(c>>4), hexChar(c&0xf))
		}
	}
	return string(out)
}

func hexChar(b byte) byte {
	if b < 10 {
		return '0' + b
	}
	return 'a' + b - 10
}
