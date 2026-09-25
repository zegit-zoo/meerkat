// Command meerkat is a knowledge-base CLI: it loads a body of Markdown
// and serves it over CLI, MCP, and HTTP.
//
// The binary carries no content of its own — it resolves a knowledge base at
// runtime (--kb-dir/MEERKAT_KB_DIR, or a content-source.yaml), and embedding
// content at build time is an optional secondary path. See internal/cli for
// the command tree and internal/kb for the content access API.
package main

import (
	"os"

	"github.com/zegit-zoo/meerkat/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
