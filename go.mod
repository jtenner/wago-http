module github.com/wago-org/http

go 1.24

require (
	github.com/wago-org/net v0.0.0-20260715213005-af1aae3dad77
	github.com/wago-org/wago v0.1.0
)

require github.com/soypat/lneto v0.0.0-20260710133615-ab1a0c735a8b // indirect

// Wago has not published v0.1.0 yet. Pin the exact production lifecycle merge
// selected by github.com/wago-org/net until that release exists.
replace github.com/wago-org/wago v0.1.0 => github.com/wago-org/wago v0.0.0-20260711052758-97e6f91e6c82
