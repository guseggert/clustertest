package process

// procRequestMessage is a request message.
// Only the first message needs to contain the command, args, env, wd, etc.
// Subsequent messages can contain only stdin bytes, for streaming stdin.
type procRequestMessage struct {
	Stdin     []byte
	StdinDone bool

	StopSendingStderr bool
	StopSendingStdout bool

	Command string
	Args    []string
	Env     []string
	WD      string
}

// procResponseMessage is a command response message.
// Only the last message of the stream will contain process exit information.
// Messages before the last may contain stdout or stderr bytes.
type procResponseMessage struct {
	Stdout     []byte
	StdoutDone bool

	Stderr     []byte
	StderrDone bool

	// Exited is true if the process exited. ExitCode and TimeMS must be provided in that case.
	Exited   bool
	ExitCode int
	TimeMS   int64
}
