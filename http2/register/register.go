// Package register self-registers the HTTP/2-only Wago extension.
package register

import (
	wagohttp "github.com/wago-org/http"
	"github.com/wago-org/http/http2"
	wago "github.com/wago-org/wago"
)

func init() {
	wago.RegisterExtension("http2", func() wago.Extension {
		network := wagohttp.New()
		if err := http2.Register(network); err != nil {
			panic("wagohttp/http2/register: " + err.Error())
		}
		return network
	})
}
