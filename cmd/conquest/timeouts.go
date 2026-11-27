package main

import "time"

// HTTP server timeouts.
const (
	// serverReadTimeout bounds reading a request, body included.
	serverReadTimeout = 15 * time.Second
	// serverWriteTimeout bounds how long the server takes to answer a
	// request, counted from its headers.
	serverWriteTimeout = 30 * time.Second
	// serverIdleTimeout bounds idle keep-alive connections.
	serverIdleTimeout = 60 * time.Second
)

// submitMargin is the time a WES run submission leaves the server to write
// its answer before serverWriteTimeout.
const submitMargin = 5 * time.Second

// submitTimeout bounds a WES run submission from the handler's start, so
// that its answer (the run ID) is written before serverWriteTimeout,
// whatever the run bucket mode. Within it, an obc run bucket stops waiting
// for its claim runbucket.DefaultOBCReserve (another 5s) before its end, to
// leave time to store the run and create its runner Job.
const submitTimeout = serverWriteTimeout - submitMargin
