// Package certpolicy holds the renewal rules of the node agent client
// certificate. It answers three questions:
//
//  1. When is the held certificate due for renewal, and has it expired?
//     (Evaluate)
//  2. What kind of outcome is a renewal error, and what is the next step?
//     (Classify, NextStep)
//  3. When does the agent try again? (Next)
//
// The package is pure. It reads no file, uses no network, reads no clock and
// has no random source of its own. The caller passes the time, the
// certificate times, the node id and a random source.
package certpolicy
