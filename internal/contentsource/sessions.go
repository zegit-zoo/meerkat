package contentsource

import (
	"fmt"
	"time"
)

// SessionsSpec is the `sessions:` block of content-source.yaml
// (meerkat-mob issue F): how retrieval sessions are bounded in time.
// Traversal limits (hops, steps, attempts) come from the tree's root
// manifest (`limits:`), or DefaultLimits for a flat deployment.
type SessionsSpec struct {
	// IdleTimeout ends a session that has been quiet this long; default
	// 120s. A session that ends this way counts as gave_up when nothing
	// was ever shown, timeout otherwise.
	IdleTimeout time.Duration `yaml:"idle_timeout,omitempty"`
}

// DefaultSessionIdleTimeout is the idle window when unset.
const DefaultSessionIdleTimeout = 120 * time.Second

// Validate checks the block and fills the default.
func (s *SessionsSpec) Validate(label string) error {
	if s == nil {
		return nil
	}
	if s.IdleTimeout < 0 {
		return fmt.Errorf("%s.idle_timeout must be >= 0", label)
	}
	if s.IdleTimeout == 0 {
		s.IdleTimeout = DefaultSessionIdleTimeout
	}
	if s.IdleTimeout < 5*time.Second {
		return fmt.Errorf("%s.idle_timeout must be at least 5s, got %s", label, s.IdleTimeout)
	}
	return nil
}
