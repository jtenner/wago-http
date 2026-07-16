// Command h2spec-server runs the repository's non-TLS HTTP/2 server endpoint
// for the pinned h2spec conformance runner.
package main

import (
	"flag"
	"log"
	"net"
	"time"

	h2 "github.com/wago-org/http/http2"
	"github.com/wago-org/http/http2/server"
)

func main() {
	address := flag.String("addr", "127.0.0.1:8080", "plain TCP prior-knowledge HTTP/2 listen address")
	flag.Parse()
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	log.Printf("HTTP/2 prior-knowledge endpoint listening on %s", listener.Addr())
	for {
		stream, err := listener.Accept()
		if err != nil {
			log.Print(err)
			continue
		}
		go serve(stream)
	}
}

func serve(stream net.Conn) {
	defer stream.Close()
	handler := server.HandlerFuncs{OnEnd: func(writer *server.ResponseWriter) {
		// Defer the tiny conformance response so a burst of streams remains
		// concurrently active long enough to exercise the advertised limit.
		time.AfterFunc(10*time.Millisecond, func() {
			if err := writer.Headers([]h2.HeaderField{{Name: ":status", Value: "200"}, {Name: "content-length", Value: "0"}}, true); err != nil {
				_ = writer.Reset(h2.ErrCodeInternal)
			}
		})
	}}
	connection, err := server.New(stream, handler, server.Options{Session: h2.SessionLimits{
		MaxConcurrentStreams:  100,
		MaxStreams:            4096,
		MaxClosedStreams:      1024,
		MaxQueuedOutputBytes:  8 << 20,
		MaxQueuedEventBytes:   8 << 20,
		EnableExtendedConnect: true,
	}})
	if err != nil {
		log.Printf("HTTP/2 setup: %v", err)
		return
	}
	if err := connection.Serve(); err != nil {
		log.Printf("HTTP/2 connection: %v", err)
	}
}
