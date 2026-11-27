package pg

import (
	"errors"

	"github.com/anuragxxd/conquest/internal/repository"
)

// Sentinel errors for repository operations.
var (
	// ErrNotFound is returned when the requested entity does not exist;
	// it is repository.ErrNotFound, for callers that know only the
	// interfaces.
	ErrNotFound = repository.ErrNotFound

	// ErrConcurrentUpdate is returned when an optimistic-locking update
	// finds that the row's state has already changed (0 rows affected).
	ErrConcurrentUpdate = errors.New("concurrent update: state already changed")

	// ErrEmptyOwner is returned when a run is created in a context scoped
	// to an empty owner ID (package ownership).
	ErrEmptyOwner = errors.New("scoped to an empty owner")
)
