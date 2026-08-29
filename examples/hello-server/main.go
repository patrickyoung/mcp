package main

import (
	"context"
	"log"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type helloInput struct {
	Name string `json:"name" jsonschema:"the person to greet"`
}

type helloOutput struct {
	Greeting string `json:"greeting"`
}

func main() {
	server := mcp.NewServer(&mcp.Implementation{Name: "unix-mcp-hello", Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "hello", Description: "greet one person"},
		func(_ context.Context, _ *mcp.CallToolRequest, input helloInput) (*mcp.CallToolResult, helloOutput, error) {
			return nil, helloOutput{Greeting: "hello, " + input.Name}, nil
		})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
