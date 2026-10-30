package factsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/httpx"
)

// This file is the per-fact wire (COR-2126): brainstore's idempotent batch
// upsert and repo-wide delta feed, published by the cell gateway under the
// same /api/v1/repos/{id}/brain/* namespace as the branch-head endpoints
// (verified live: the bare path 404s, /api/v1/brain/v1/health answers with
// brainstore's health body).

// ErrInvalidPageToken is returned when the feed rejects the stored cursor
// (400 with a token sent) — the server's keyset epoch moved. The caller
// resets its cursor and cache and refetches from the start.
var ErrInvalidPageToken = errors.New("factsync: hosted feed rejected the page token")

// ErrNoAccess is returned on 403: the bound repo is gone or this caller lost
// its grant (brainstore deliberately maps a deleted repo to 403 too, so the
// two are indistinguishable here). The caller stops retrying the binding
// instead of hammering the same denial (COR-2279). 401 stays a plain error —
// an auth-plumbing break should keep reaching the server-side monitor.
var ErrNoAccess = errors.New("access denied")

// UpsertCounts is the facts upsert response: how the batch landed.
type UpsertCounts struct {
	Added     int `json:"added"`
	Changed   int `json:"changed"`
	Unchanged int `json:"unchanged"`
}

// FeedItem is one delta-feed entry: the wire record (entire-brain factmerge
// encoding, snake_case) wrapped in the server-owned camelCase envelope.
type FeedItem struct {
	Record          factmerge.Record `json:"record"`
	TrailID         string           `json:"trailId"`
	CreatedBy       string           `json:"createdBy"`
	CreatedByHandle string           `json:"createdByHandle"`
	UpdatedAt       string           `json:"updatedAt"`
}

// FeedPage is one page of the delta feed. Every non-empty page carries the
// resume token positioned after its last item; only an empty page is
// terminal-for-now (nil token).
type FeedPage struct {
	Items         []FeedItem `json:"items"`
	NextPageToken *string    `json:"nextPageToken"`
}

// brainPath builds the per-fact route base on repoBasePath, the package's
// single point where a repo id becomes part of a request target.
func brainPath(repoID string) (string, error) {
	base, err := repoBasePath(repoID)
	if err != nil {
		return "", err
	}
	return base + "/brain", nil
}

// UpsertFacts posts one NDJSON batch to the idempotent per-fact upsert. The
// caller chunks to the server's batch cap and verifies identities first —
// one bad record rejects the whole batch server-side.
func (h *HTTPServer) UpsertFacts(ctx context.Context, repoID string, ndjson []byte) (UpsertCounts, error) {
	base, err := brainPath(repoID)
	if err != nil {
		return UpsertCounts{}, err
	}
	// POST on the facts collection is the batch upsert (COR-2212 replaced the
	// facts:upsert colon path); requires a brainstore that serves it.
	req, err := h.newRequest(ctx, http.MethodPost, base+"/facts", bytes.NewReader(ndjson))
	if err != nil {
		return UpsertCounts{}, err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := h.do(req)
	if err != nil {
		return UpsertCounts{}, fmt.Errorf("factsync: POST facts upsert %s: %w", repoID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return UpsertCounts{}, fmt.Errorf("factsync: POST facts upsert %s: %w: status %s%s", repoID, ErrNoAccess, resp.Status, h.detail(resp))
	}
	if resp.StatusCode != http.StatusOK {
		return UpsertCounts{}, fmt.Errorf("factsync: POST facts upsert %s: unexpected status %s%s", repoID, resp.Status, h.detail(resp))
	}
	var counts UpsertCounts
	if err := httpx.DecodeJSONBody(resp, &counts); err != nil {
		return UpsertCounts{}, fmt.Errorf("factsync: decode facts upsert %s: %w", repoID, err)
	}
	return counts, nil
}

// FactsFeedPage reads one page of the repo-wide delta feed. pageToken ""
// starts from the beginning; pageSize is clamped server-side.
func (h *HTTPServer) FactsFeedPage(ctx context.Context, repoID, pageToken string, pageSize int) (FeedPage, error) {
	base, err := brainPath(repoID)
	if err != nil {
		return FeedPage{}, err
	}
	query := url.Values{}
	if pageToken != "" {
		query.Set("pageToken", pageToken)
	}
	if pageSize > 0 {
		query.Set("pageSize", strconv.Itoa(pageSize))
	}
	path := base + "/facts"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := h.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return FeedPage{}, err
	}
	resp, err := h.do(req)
	if err != nil {
		return FeedPage{}, fmt.Errorf("factsync: GET facts feed %s: %w", repoID, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusForbidden:
		return FeedPage{}, fmt.Errorf("factsync: GET facts feed %s: %w: status %s%s", repoID, ErrNoAccess, resp.Status, h.detail(resp))
	case resp.StatusCode == http.StatusBadRequest && pageToken != "":
		return FeedPage{}, fmt.Errorf("%w%s", ErrInvalidPageToken, h.detail(resp))
	default:
		return FeedPage{}, fmt.Errorf("factsync: GET facts feed %s: unexpected status %s%s", repoID, resp.Status, h.detail(resp))
	}
	var page FeedPage
	if err := httpx.DecodeJSONBody(resp, &page); err != nil {
		return FeedPage{}, fmt.Errorf("factsync: decode facts feed %s: %w", repoID, err)
	}
	return page, nil
}
