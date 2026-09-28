// Package sdkerr gives every SDK package (consumer, producer, agent,
// credentials) one place to turn an upstream failure (a cloud SDK call, an
// HTTP client request, a compression/decompression library call) into a
// customer-facing error: an authored, capability-language message that never
// interpolates the upstream error's own text (which can carry account IDs,
// key ARNs, queue URLs, or other internal service detail), while still
// attaching the original error as the cause so errors.Is / errors.As and a
// developer's own error-message logging can reach it.
package sdkerr

// Wrap returns an error whose Error() is exactly msg — cause's text is never
// interpolated into it — and whose Unwrap() returns cause, so errors.Is and
// errors.As still traverse to the original upstream error.
func Wrap(msg string, cause error) error {
	return &wrapped{msg: msg, cause: cause}
}

type wrapped struct {
	msg   string
	cause error
}

func (w *wrapped) Error() string { return w.msg }
func (w *wrapped) Unwrap() error { return w.cause }

// WrapSentinel returns an error whose Error() is exactly sentinel's message
// and whose errors.Is/errors.As reach BOTH sentinel (so an existing exported
// error category keeps matching, e.g. a package-level "not compressed"
// marker) AND cause (the raw upstream error, for debugging) — via the
// multi-error Unwrap() []error form errors.Is/As have followed since Go 1.20.
func WrapSentinel(sentinel, cause error) error {
	return &sentinelWrapped{sentinel: sentinel, cause: cause}
}

type sentinelWrapped struct {
	sentinel error
	cause    error
}

func (w *sentinelWrapped) Error() string   { return w.sentinel.Error() }
func (w *sentinelWrapped) Unwrap() []error { return []error{w.sentinel, w.cause} }
