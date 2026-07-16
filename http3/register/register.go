// Package register self-registers the HTTP/3-only Wago extension.
package register

import (
	wagohttp "github.com/wago-org/http"
	"github.com/wago-org/http/http3"
	wago "github.com/wago-org/wago"
)

func init() {
	wago.RegisterExtension("http3", func() wago.Extension {
		network := wagohttp.New()
		if err := http3.Register(network); err != nil {
			panic("wagohttp/http3/register: " + err.Error())
		}
		return network
	})
}
