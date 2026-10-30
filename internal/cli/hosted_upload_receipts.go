package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

// Receipts contain hashes only, never memory bodies or provenance. The store
// itself is the durable pending set: any version without an acknowledgement
// remains eligible on the next event. A failed request never advances a receipt.
type hostedUploadReceipts struct {
	Version  int             `json:"schema_version"`
	Versions map[string]bool `json:"versions"`
}

func hostedUploadHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hostedUploadReceiptPath(baseURL, repoID string) string {
	return "hosted_upload/" + hostedUploadHash([]byte(baseURL+"\x00"+repoID)) + ".json"
}

func loadHostedUploadReceipts(brainDir, rel string) (hostedUploadReceipts, error) {
	r := hostedUploadReceipts{Version: 1, Versions: map[string]bool{}}
	data, present, err := brainstore.ReadMemoryStateFile(brainDir, rel, "upload receipts", 64<<20)
	if err != nil || !present {
		return r, err
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, fmt.Errorf("read upload receipts: %w", err)
	}
	if r.Version != 1 || r.Versions == nil {
		return r, errors.New("unsupported upload receipts")
	}
	return r, nil
}

func saveHostedUploadReceipts(brainDir, rel string, r hostedUploadReceipts) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return brainstore.WriteBrainRelativeFileAtomic(brainDir, rel, append(data, '\n'), 0o600)
}

// Serialize uploads across processes, including manual sync. Unlike the old
// time throttle, a waiting completion event reads the store AFTER its predecessor
// finishes. The lock does not block memory writers and is released on process exit.
func acquireHostedUploadLock(ctx context.Context, brainDir string) (*brainstore.FileLock, error) {
	if err := base.RejectSymlinkedBrainRoot(brainDir); err != nil {
		return nil, err
	}
	if err := base.RejectExistingSymlinkPathComponents(brainDir, brainstore.BrainLockDirName); err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lock, err := brainstore.AcquireFileLock(filepath.Join(brainDir, brainstore.BrainLockDirName, "hosted-upload.lock"), "hosted_upload_locked", 0)
		if !errors.Is(err, brainstore.ErrFileLockTimeout) {
			return lock, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
