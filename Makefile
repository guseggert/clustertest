.PHONY: nodeagent
nodeagent:
	GOOS=linux GOARCH=amd64 go build -o nodeagent ./cmd/agent/main.go

