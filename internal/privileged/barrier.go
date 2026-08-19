//go:build !camptest

package privileged

// barrier does nothing, and in a camp anybody runs it does not exist.
//
// The measurements the review asks for -- killing the helper at each
// boundary, and swapping the environment's name under it at each
// resolution -- need the privileged half to stop at a named point. That
// is a real need and it has a real cost: a process that can be paused at
// a chosen instant, by whoever owns the directory it works in, is exactly
// the primitive those measurements exist to prove camp is safe from. A
// pause the invoking user can trigger inside the root helper would not be
// a test seam. It would be the attack.
//
// So it is not a variable something can reassign, and not a file check
// that a production binary makes and happens to find nothing at. The body
// with the protocol in it is compiled only under -tags camptest, and this
// one -- the empty one -- is what every ordinary build has. There is
// nothing to disable, nothing to leave switched on by accident, and
// nothing in the shipped binary to find.
//
// See barrier_camptest.go for what the other build does and how to drive
// it.
func barrier(Job, string) {}
