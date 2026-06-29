// Command agentcarto-plugin-codex is the executable that serves AgentCarto's
// Codex plugin as a subprocess. The host (agentcarto) launches it as a child
// process and communicates over net/rpc. Running it standalone exits after a
// failed handshake, as required by go-plugin.
package main

import (
	"github.com/agentcarto/core/plugin"
	"github.com/agentcarto/plugin-codex"
)

func main() {
	plugin.Serve(codex.Factory{})
}
