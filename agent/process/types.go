package process

import "syscall"

type procReq struct {
	Command string
	Args    []string
	Env     []string
	WD      string
	Stdin   fdConfig
	Stdout  fdConfig
	Stderr  fdConfig
}

type fdConfig struct {
	Discard bool
	File    string
}

type fdPayload struct {
	B    []byte
	Done bool
}

// procRequestMessage is a request message.
// Only the first message needs to contain the command, args, env, wd, etc.
// Subsequent messages can contain only stdin bytes, for streaming stdin.
type procRequestMessage struct {
	Stdin  fdPayload
	Signal syscall.Signal
	Req    *procReq
}

type procResult struct {
	// Exited is true if the process exited. ExitCode and TimeMS must be provided in that case.
	Exited   bool
	ExitCode int
	TimeMS   int64
}

// procResponseMessage is a command response message.
// Only the last message of the stream will contain process exit information.
// Messages before the last may contain stdout or stderr bytes.
type procResponseMessage struct {
	Stdout fdPayload
	Stderr fdPayload
	Result procResult
}
