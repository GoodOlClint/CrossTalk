// Command installfakemcp serves the CrossTalk tool names at a wrong version,
// for the install handshake check.
package main

import (
	"context"
	"log"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	s := mcp.NewServer(&mcp.Implementation{Name: "crosstalk", Version: "0.0.0-fake"}, nil)
	for _, n := range []string{"list_agents", "send_message", "reply", "ask", "receive_message", "claim", "release"} {
		mcp.AddTool(s, &mcp.Tool{Name: n}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return nil, nil, nil
		})
	}
	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
