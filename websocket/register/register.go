// Package register self-registers the WebSocket-only Wago extension.
package register

import (
	wagohttp "github.com/wago-org/http"
	"github.com/wago-org/http/websocket"
	wago "github.com/wago-org/wago"
)

func init() {
	wago.RegisterExtension("websocket", func() wago.Extension {
		network := wagohttp.New()
		if err := websocket.Register(network); err != nil {
			panic("wagohttp/websocket/register: " + err.Error())
		}
		return network
	})
}
