# audiobookshelf-hardcover-sync

App for syncing reading progress from AudioBookshelf (ABS) to Hardcover.

Build with claude

Improving on:
- https://github.com/rohit-purandare/ShelfBridge
- https://github.com/drallgood/audiobookshelf-hardcover-sync

---

## Stack

- **Language:** Go
- **UI:** HTMX + templ (pure Go, no JS build step), served in browser
- **Storage:** SQLite
- **Deploy:** Docker Compose

---

## Configuration (environment variables)

| Variable | Description | Example |
|---|---|---|
| `ABS_URL` | AudioBookshelf server URL | `http://192.168.1.10:13378` |
| `ABS_TOKEN` | ABS API token | |
| `HARDCOVER_TOKEN` | Hardcover API token | |
| `CRON_SCHEDULE` | Sync cron schedule | `0 3 * * *` |
| `CRON_TIMEZONE` | Timezone for cron | `Europe/Prague` |

Single-user: one ABS account, one Hardcover account.

---

## UI

Two-column layout:
- **Left:** book from AudioBookshelf (cover, title, author, progress, link to ABS)
- **Right:** matched Hardcover edition (cover, title, link to Hardcover)

Books are grouped into four categories, newest-first within each:

| # | Category | Description |
|---|---|---|
| 1 | **Unmatched** | ABS books with no Hardcover edition ID yet |
| 2 | **Needs sync** | Matched books where ABS progress advanced ≥ 2 min since last sync |
| 3 | **Synced** | Matched and up-to-date |
| 4 | **Ignored** | Explicitly ignored; can be un-ignored |

---

## Sync lifecycle

```
ABS book
  │
  ├─ edition ID known? ──yes──► progress changed by ≥2 min? ──yes──► push update to Hardcover
  │                                                           └──no───► no-op
  │
  └─ no ──► ignored? ──yes──► no-op
            │
            └─ no ──► auto-match (ISBN → ASIN → author+title)
                        │
                        ├─ match found ──► show candidates with match reason for confirmation
                        │                  └─ user confirms ──► insert_user_book in Hardcover
                        │                                        set status: reading or finished
                        │
                        └─ no match ──► show link to Hardcover edition search
                                        user pastes edition ID ──► same as above
```

Sync runs on cron schedule or manually via UI button.

### Status mapping

| ABS state | Hardcover status_id |
|---|---|
| in progress | 2 (Currently Reading) |
| finished | 3 (Read) |

**Important:** never create new books in Hardcover. Only work with editions that already exist there.

### Re-reads

Detection: ABS progress resets to near-zero after previously being near-finished.
Flow: show confirmation in UI → user confirms → `insert_user_book_read` (new session) + set status back to 2.

---

## Book covers

- **ABS covers:** served at `{ABS_URL}/api/items/{id}/cover` — requires auth, so the app exposes a proxy endpoint `GET /proxy/abs-cover/{itemId}` that forwards the request server-side.
- **Hardcover covers:** public URL returned by the Hardcover API, used directly in `<img src>`.

---

## API reference

### AudioBookshelf REST API

Base URL: `$ABS_URL`, auth: `Authorization: Bearer {ABS_TOKEN}`

| Endpoint | Purpose |
|---|---|
| `GET /api/libraries` | List libraries |
| `GET /api/libraries/{id}/items` | All books |
| `GET /api/items/{id}` | Single book details |
| `GET /api/me/progress/{itemId}` | Progress for a book |
| `GET /api/me/items-in-progress` | All in-progress books |

Key progress fields: `currentTime` (seconds), `progress` (0–1), `isFinished`, `lastUpdate`

Key metadata fields: `media.metadata.{title, authorName, isbn, asin}`

### Hardcover GraphQL API

Endpoint: `https://api.hardcover.app/v1/graphql`, auth: `Authorization: Bearer {HARDCOVER_TOKEN}`

| Mutation | Purpose | Key inputs |
|---|---|---|
| `insert_user_book` | Add book to user library | `book_id`, `edition_id`, `status_id` |
| `insert_user_book_read` | Start new reading session (also re-reads) | `user_book_id`, `edition_id`, `started_at`, `progress_seconds` |
| `update_user_book_read` | Update progress in existing session | `id`, `progress_seconds`, `finished_at` |
| `update_user_book` | Change book status | `id`, `status_id` |

Status IDs: `1` = Want to Read, `2` = Currently Reading, `3` = Read

Reading format IDs: `2` = Audiobook, `4` = Ebook

Matching queries: filter by `isbn_13`/`isbn_10`, `asin`, or `search(query_type: "books")`
