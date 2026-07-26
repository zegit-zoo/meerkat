// Command meerkat is a knowledge-base CLI: it embeds a body of Markdown
// and serves it over CLI, MCP, and HTTP.
//
// All wiki content is embedded into the binary at build time from the
// configured content source. See internal/cli for the command tree and
// internal/kb for the content access API.
package main

import (
	"os"

	"github.com/zegit-zoo/meerkat/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
