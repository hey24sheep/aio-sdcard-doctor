// Package source declares what the actions read from: a card's files (OS folder or built-in
// parser) or its raw device. The errors here are the reader's failure kinds every action handles.
package source

import "errors"

var (
	// ErrStalled: one read took longer than the stall timeout; the worker was restarted.
	ErrStalled = errors.New("the card stopped responding to a read")
	// ErrWorkerCrashed: the reader process died (parser crash, memory cap); it was restarted.
	ErrWorkerCrashed = errors.New("the card reader process crashed")
	// ErrTooManyRestarts: the restart budget (5 per 10 minutes) is spent; ask the user.
	ErrTooManyRestarts = errors.New("the card keeps failing; the reader was restarted too often")
	// ErrDifferentCard: after a re-plug or a reader restart, a different card is at the path.
	ErrDifferentCard = errors.New("a different card is in the reader — put the original card back")
	// ErrLocked: another program holds the file open (Windows sharing violation, EBUSY).
	ErrLocked = errors.New("the file is in use by another program")
)
