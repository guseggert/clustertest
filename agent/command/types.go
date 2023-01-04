package command

// commandRequestMessage is a request message.
// Only the first message needs to contain the command, args, env, wd, etc.
// Subsequent messages can contain only stdin bytes, for streaming stdin.
type commandRequestMessage struct {
	Stdin     []byte
	StdinDone bool

	StopSendingStderr bool
	StopSendingStdout bool

	Command string
	Args    []string
	Env     []string
	WD      string
}

// commandResponseMessage is a command response message.
// Only the last message of the stream will contain process exit information.
// Messages before the last may contain stdout or stderr bytes.
type commandResponseMessage struct {
	Stdout     []byte
	StdoutDone bool

	Stderr     []byte
	StderrDone bool

	Exited   bool
	ExitCode int

	Err string
}
