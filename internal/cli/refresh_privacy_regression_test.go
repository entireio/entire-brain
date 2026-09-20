package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHistoryFreshnessRejectsCorruptPrivacyWithoutPanic(t *testing.T) {
	b := writePrivacyFixture(t)
	m, e := loadBrainManifest(b)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(b, sessionTombstonesPath), []byte("{broken"), 0600); e != nil {
		t.Fatal(e)
	}
	if historyIndexCurrent(b, m) {
		t.Fatal("corrupt policy accepted as current")
	}
	if _, err := writeBrainHistoryIndexAndSourceContext(context.Background(), b, time.Now(), nil); err == nil {
		t.Fatal("rebuild ignored corrupt policy")
	} else {
		var policyErr *sessionTombstoneLoadError
		if !errors.As(err, &policyErr) {
			t.Fatalf("expected typed privacy failure, got %T %v", err, err)
		}
		t.Logf("typed rebuild error: %T %v", err, err)
	}
}
