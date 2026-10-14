package store

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

// SetProviderAlias changes presentation only. It never changes attribution,
// endpoint identity, historical prices, or the pricing fingerprint.
// An empty alias restores the normalized endpoint label.
func (s *Store) SetProviderAlias(ctx context.Context, id, alias string) error {
	for _, r := range alias {
		if unicode.IsControl(r) {
			return fmt.Errorf("provider alias contains a control character")
		}
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM provider_endpoints WHERE provider_id=?)", id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("unknown provider identity")
	}
	if err := s.SetSetting(ctx, "provider.alias."+id, strings.TrimSpace(alias)); err != nil {
		return err
	}
	s.notify()
	return nil
}
