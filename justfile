default:
    go test ./...

race:
    go test -race ./...

build:
    mkdir -p bin
    go build -o bin/mcp ./cmd/mcp
    go build -o bin/mcp-legacy ./cmd/mcp-legacy
    go build -o bin/mcpbox ./cmd/mcpbox
    go build -o bin/mcpserve ./cmd/mcpserve
