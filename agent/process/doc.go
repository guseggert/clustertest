/*
Package process provides a client and server for a remote process runner which streams stdin (client->server) and stdout & stderr (server->client). It uses WebSockets for bidi messaging so only requires an HTTPS server.

Processes are scoped to the WebSocket connection--that is, if the connection dies for any reason, the process is killed. If you want to run a process that survives across connections, then run it as a background process with disk/pipe buffering of stdin/stdout/stderr.

There are two messages in this protocol: "request" messages are sent client->server, and "response" messages are sent server->client. The schema for these messages is described in types.go.

The protocol proceeds as follows:

1. The client opens a WebSocket connection with the server
2. The client sends a request message containing the Command and Args fields, and optionally the Env and WD fields.
3. The client and server then exchange messages containing stdin, stdout, and stderr bytes while the process runs.
4. When the process exits, the server sends a response message with Exited=true and the ExitCode.
5. The client initiates closing of the WebSocket connection.

The server does not buffer any stdout or stderr, which generally means that the client must read them to completion before the process will exit cleanly.

Signaling is not implemented, but should be easy to add if the use case arises.
*/
package process
