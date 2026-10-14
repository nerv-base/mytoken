package scan

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/quota"
	"github.com/zzstar101/mytoken/internal/store"
)

// publishQuota persists the quota observation carried by batches that have
// already committed, one bounded latest observation per instance.
//
// It is called only after CommitMany succeeded, so a quota observation can
// never be visible for a batch whose events and cursors were rolled back.
// Writes are grouped by instance: several sources of one Codex instance (a
// rollout and its archived copy, say) reduce to one observation via
// quota.Merge, which never lets a newer unusable record make an older
// observation look fresh. The key is per instance, so two instances never
// share or overwrite each other's quota.
//
// Only Codex publishes quota today, which is what the quota setting namespace
// is named for; another harness would need its own prefix rather than reuse
// this one.
func (s *Scanner) publishQuota(ctx context.Context, writes []store.Write) error {
	if s.st == nil || len(writes) == 0 {
		return nil
	}
	now := time.Now()
	instances := make([]string, 0, len(writes))
	merged := make(map[string]*quota.Report, len(writes))
	for _, w := range writes {
		if w.Batch.Quota == nil || w.Harness != model.Codex {
			continue
		}
		id := store.NormalizeInstanceID(w.InstanceID)
		cur, ok := merged[id]
		if !ok {
			instances = append(instances, id)
			merged[id] = w.Batch.Quota
			continue
		}
		merged[id] = quota.Merge(cur, w.Batch.Quota)
	}
	var errs []error
	for _, id := range instances {
		if err := quota.PublishLatest(ctx, s.st, id, merged[id], now); err != nil {
			errs = append(errs, fmt.Errorf("publish codex quota for instance %q: %w", id, err))
		}
	}
	return errors.Join(errs...)
}
