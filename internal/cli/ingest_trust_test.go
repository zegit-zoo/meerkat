package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// --trust-sources on a --role run needs --trust-intake as well, and
// warns about caller-supplied text when both are given (meerkat-mob#34).
func TestCheckRoleTrust(t *testing.T) {
	cases := []struct {
		name          string
		sources, take bool
		wantErr       bool
		wantWarn      bool
	}{
		{"neither", false, false, false, false},
		{"intake alone does nothing", false, true, false, false},
		{"sources alone is refused", true, false, true, false},
		{"both warn", true, true, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var errOut bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetErr(&errOut)
			err := checkRoleTrust(cmd, ingestFlags{role: "researcher", trustSources: c.sources, trustIntake: c.take})
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v; want error %v", err, c.wantErr)
			}
			if c.wantErr && !strings.Contains(err.Error(), "--trust-intake") {
				t.Errorf("refusal does not name the second flag: %v", err)
			}
			warned := strings.Contains(errOut.String(), "intake deposits")
			if warned != c.wantWarn {
				t.Errorf("warning = %q; want warned %v", errOut.String(), c.wantWarn)
			}
		})
	}
}
