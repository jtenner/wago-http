// Package register self-registers the aggregate HTTP/1.1, HTTP/2, WebSocket,
// and HTTP/3 Wago extension.
package register

import (
	wagohttp "github.com/wago-org/http"
	http1 "github.com/wago-org/http/http"
	"github.com/wago-org/http/http2"
	"github.com/wago-org/http/http3"
	"github.com/wago-org/http/websocket"
	wago "github.com/wago-org/wago"
)

func init() {
	wago.RegisterExtension("http-all", func() wago.Extension {
		network := wagohttp.New()
		registrations := []func(*wagohttp.Network) error{
			http1.Register,
			http2.Register,
			websocket.Register,
			http3.Register,
		}
		for _, register := range registrations {
			if err := register(network); err != nil {
				panic("wagohttp/register: " + err.Error())
			}
		}
		return network
	})
}
