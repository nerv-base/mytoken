package scan

import (
	"errors"
	"fmt"

	"github.com/zzstar101/mytoken/internal/attrib"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/pricing"
	"github.com/zzstar101/mytoken/internal/store"
)

// Instance is one scannable configuration: a parser bound to the instance id
// its events, cursors and sessions are recorded under. One harness may appear
// several times under different ids; one id may appear once per harness.
type Instance struct {
	// ID identifies the instance in the store. Empty means the store's
	// default instance.
	ID string
	// Parser discovers and parses this instance's sources. It must not be nil.
	Parser harness.Parser
}

var (
	// ErrNilParser reports an Instance that carries no parser.
	ErrNilParser = errors.New("scan: instance has no parser")
	// ErrDuplicateInstance reports two instances with the same id under the
	// same harness. Cursors and event dedup keys are keyed by harness and
	// instance id, so such a pair would silently share one checkpoint and
	// count one of the two instances' events as a duplicate of the other's.
	ErrDuplicateInstance = errors.New("scan: duplicate instance id for harness")
)

// NewWithInstances returns a Scanner over exactly the given instances, in the
// order given. Instances with an empty ID are recorded under the store's
// default instance.
//
// It reports a bad configuration rather than repairing it: a nil parser is
// ErrNilParser and a repeated id under one harness is ErrDuplicateInstance.
// An empty list scans nothing — it never falls back to the registered
// harnesses the way New does.
func NewWithInstances(st *store.Store, resolver *attrib.Resolver, pricer *pricing.Pricer, instances ...Instance) (*Scanner, error) {
	s := newScanner(st, resolver, pricer)
	seen := map[string]bool{}
	for _, inst := range instances {
		if inst.Parser == nil {
			return nil, fmt.Errorf("%w (id %q)", ErrNilParser, inst.ID)
		}
		id := store.NormalizeInstanceID(inst.ID)
		key := string(inst.Parser.Harness()) + "\x00" + id
		if seen[key] {
			return nil, fmt.Errorf("%w: harness %s id %q", ErrDuplicateInstance, inst.Parser.Harness(), id)
		}
		seen[key] = true
		s.instances = append(s.instances, Instance{ID: id, Parser: inst.Parser})
	}
	return s, nil
}

// Instances returns a copy of the scanner's instance list, so a caller can
// report status under the same ids the scanner is writing.
func (s *Scanner) Instances() []Instance {
	out := make([]Instance, len(s.instances))
	copy(out, s.instances)
	return out
}
