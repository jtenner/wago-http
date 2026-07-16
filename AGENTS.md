This repository provides capability-gated HTTP-family plugins for the Wago WebAssembly runtime, built on `github.com/wago-org/net`. It keeps HTTP/1.1, HTTP/2, WebSocket, and HTTP/3 independently selectable while sharing bounded networking and lifecycle infrastructure.

## Goals

- Keep the API simple, fast, and as allocation-free as reasonably possible.
- Keep commits atomic and bounded.
- Treat correctness and speed as paramount.
