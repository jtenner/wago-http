// Package register self-registers the HTTP/1.1-only Wago extension.
package register

import (
	wagohttp "github.com/wago-org/http"
	http1 "github.com/wago-org/http/http"
	wago "github.com/wago-org/wago"
)

func init() {
	wago.RegisterExtension("http", func() wago.Extension {
		network := wagohttp.New()
		if err := http1.Register(network); err != nil {
			panic("wagohttp/http/register: " + err.Error())
		}
		return network
	})
}
