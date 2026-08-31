package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// The byte-less Item (§11, ADR-0056, M12).
//
// An Item is a part of a Work that a source emitted — a single episode — that
// nobody has bytes for yet. It sits between Edition and Asset in the content
// spine: an Asset is a file that EXISTS, and there was no entity for "the
// episode that should exist and does not" until this one. Acquiring an Item
// produces an Asset and a Blob the ordinary way; until then it is a row that
// carries WHAT the source said, and no bytes.

// Item is a stored byte-less Item.
type Item struct {
	ID          string
	WorkID      string
	EditionID   string
	ItemKey     string
	Title       string
	PublishedAt time.Time
	Attributes  map[string]string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// UpsertItem records an Item a feed adapter enumerated, keyed by its
// source-stable (work, item_key) identity, and reports whether the row was newly
// created.
//
// # Idempotent by the source-stable key (invariant 9)
//
// A poll re-enumerates a feed and re-presents the same keys; a key already
// stored is not a new Item, so the upsert refreshes the mutable facts (title,
// air date, attributes) and reports created=false. Only a first sighting reports
// created=true and emits content.item.discovered — a re-poll that changes
// nothing must not put an event in the log per episode per six hours.
//
// The mutable facts ARE refreshed on a re-sight because a feed corrects itself —
// an air date that was a placeholder becomes real, a title is fixed — and the
// Item should track the source rather than freeze at first sighting.
func (c *Catalog) UpsertItem(
	ctx context.Context, workID string, fi followed.FeedItem,
) (Item, bool, error) {
	if err := fi.Validate(); err != nil {
		return Item{}, false, fmt.Errorf("catalog: %w", err)
	}
	if workID == "" {
		return Item{}, false, errors.New("catalog: an item must name the work it belongs to")
	}
	attrs := fi.Attributes
	if attrs == nil {
		attrs = map[string]string{}
	}
	encoded, err := json.Marshal(attrs)
	if err != nil {
		return Item{}, false, fmt.Errorf("catalog: encoding item attributes: %w", err)
	}
	now := c.clock.Now().UTC()
	stamp := now.Format(timestampFormat)
	var published any
	if !fi.PublishedAt.IsZero() {
		published = fi.PublishedAt.UTC().Format(timestampFormat)
	}

	var (
		out     Item
		created bool
		ev      events.Event
	)
	err = c.db.InTx(ctx, func(tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM items WHERE work_id = ? AND item_key = ?`, workID, fi.Key).
			Scan(&id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			id = uuid.Must(uuid.NewV7()).String()
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO items
					(id, work_id, edition_id, item_key, title, published_at, attributes,
					 created_at, updated_at)
				VALUES (?, ?, NULL, ?, ?, ?, ?, ?, ?)`,
				id, workID, fi.Key, fi.Title, published, string(encoded), stamp, stamp); err != nil {
				return fmt.Errorf("catalog: inserting an item: %w", err)
			}
			created = true
			ev, err = c.events.EmitTx(ctx, tx, events.TypeItemDiscovered, "item", id,
				map[string]any{
					"item_id":  id,
					"work_id":  workID,
					"item_key": fi.Key,
					"title":    fi.Title,
				})
			if err != nil {
				return err
			}
		case err != nil:
			return fmt.Errorf("catalog: reading an item: %w", err)
		default:
			if _, err := tx.ExecContext(ctx, `
				UPDATE items
				   SET title = ?, published_at = ?, attributes = ?, updated_at = ?
				 WHERE id = ?`,
				fi.Title, published, string(encoded), stamp, id); err != nil {
				return fmt.Errorf("catalog: updating an item: %w", err)
			}
		}
		out = Item{
			ID: id, WorkID: workID, ItemKey: fi.Key, Title: fi.Title,
			PublishedAt: fi.PublishedAt, Attributes: attrs, CreatedAt: now, UpdatedAt: now,
		}
		return nil
	})
	if err != nil {
		return Item{}, false, err
	}
	if created {
		c.events.Publish(ev)
	}
	return out, created, nil
}

// ItemsForWork lists a work's known Items in source-key order. It answers
// "how many items does this source have" for the followed-sources listing, and
// is the read half of the poll diff for anything that wants the stored set
// rather than upserting one at a time.
func (c *Catalog) ItemsForWork(ctx context.Context, workID string) ([]Item, error) {
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT id, work_id, coalesce(edition_id, ''), item_key, title,
		       coalesce(published_at, ''), attributes, created_at, updated_at
		FROM items WHERE work_id = ? ORDER BY item_key`, workID)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing items for a work: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Item
	for rows.Next() {
		var (
			it               Item
			published, attrs string
			created, updated string
		)
		if err := rows.Scan(&it.ID, &it.WorkID, &it.EditionID, &it.ItemKey, &it.Title,
			&published, &attrs, &created, &updated); err != nil {
			return nil, fmt.Errorf("catalog: reading an item: %w", err)
		}
		if published != "" {
			it.PublishedAt, _ = time.Parse(timestampFormat, published)
		}
		it.Attributes = map[string]string{}
		if err := json.Unmarshal([]byte(attrs), &it.Attributes); err != nil {
			return nil, fmt.Errorf("catalog: decoding item attributes: %w", err)
		}
		it.CreatedAt, _ = time.Parse(timestampFormat, created)
		it.UpdatedAt, _ = time.Parse(timestampFormat, updated)
		out = append(out, it)
	}
	return out, rows.Err()
}
