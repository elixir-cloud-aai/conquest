package main

import (
	"testing"
	"time"

	"github.com/anuragxxd/conquest/internal/runbucket"
)

// A run submission answers before the server's write deadline, and an obc
// claim gets all of its bind timeout within the submission.
func TestSubmitTimeoutFitsTheWriteTimeout(t *testing.T) {
	if submitTimeout >= serverWriteTimeout {
		t.Fatalf("submitTimeout %s must stay below the server's write timeout %s", submitTimeout, serverWriteTimeout)
	}
	bindable := submitTimeout - runbucket.DefaultOBCReserve
	if bindable < 10*time.Second {
		t.Fatalf("submitTimeout %s leaves an obc claim less than 10s to bind", submitTimeout)
	}
	if runbucket.MaxOBCBindTimeout > bindable {
		t.Fatalf("MaxOBCBindTimeout %s can't be used: a submission leaves a claim %s", runbucket.MaxOBCBindTimeout, bindable)
	}
	if runbucket.DefaultOBCBindTimeout > runbucket.MaxOBCBindTimeout {
		t.Fatalf("DefaultOBCBindTimeout %s above the maximum %s", runbucket.DefaultOBCBindTimeout, runbucket.MaxOBCBindTimeout)
	}
}
