/*
Package command provides a client and server for a remote process runner which streams stdin (client->server) and stdout & stderr (server->client). It uses WebSockets for bidi messaging so only requires an HTTPS server.

Processes are scoped to the WebSocket connection--that is, if the connection dies for any reason, the process is killed. If you want to run a process that survives across connections, then run it as a background process.
*/
package command
