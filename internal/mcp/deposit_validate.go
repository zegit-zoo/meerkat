package mcp

import (
	"fmt"

	"github.com/zegit-zoo/meerkat/internal/intake"
)

// deposit_validate.go holds the checks mk_report_outcome applies to what
// a caller deposits for the intake pipeline, kept apart from the tool's
// argument plumbing in outcome.go.

// validateDepositSource refuses a fallback source the researcher agent
// must not be pointed at: anything but https to a public host
// (intake.CheckSource; meerkat-mob#34).
func validateDepositSource(s string) error {
	if err := intake.CheckSource(s); err != nil {
		return fmt.Errorf("fallback.sources entry %q is refused: %v; only https URLs to public hosts are accepted", s, err)
	}
	return nil
}
