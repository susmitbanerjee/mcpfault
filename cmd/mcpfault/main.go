// Command mcpfault tests how AI agents recover when their MCP tools fail.
package main

import (
	"os"

	"github.com/mcpfault/mcpfault/internal/cli"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3"
var version = "dev"

func main() {
	os.Exit(cli.Execute(version))
}
