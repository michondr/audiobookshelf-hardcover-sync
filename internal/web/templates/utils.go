package templates

import (
	"fmt"
	"strings"
	"time"

	"github.com/michondr/audiobookshelf-hardcover-sync/internal/db"
)

// BookGroups splits a flat book list into the four display categories.
type BookGroups struct {
	NeedsAction []db.Book // searched but no auto-match: candidates to pick or manual input needed
	Matched     []db.Book // confirmed edition
	Pending     []db.Book // not yet searched
	Ignored     []db.Book
}

func groupBooks(books []db.Book) BookGroups {
	var g BookGroups
	for _, b := range books {
		switch {
		case b.HCIgnored:
			g.Ignored = append(g.Ignored, b)
		case b.HCEditionID != nil:
			g.Matched = append(g.Matched, b)
		case b.HCMatchSearchedAt != nil:
			g.NeedsAction = append(g.NeedsAction, b)
		default:
			g.Pending = append(g.Pending, b)
		}
	}
	return g
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
