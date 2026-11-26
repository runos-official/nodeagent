// Package certrenew runs the renewal loop of the node agent client
// certificate. One loop runs for the life of the agent process. A reconnect
// of the instruction stream does not start, stop or reset the loop.
//
// The loop asks the policy package (uc/certpolicy) when to act and how to
// read a refusal. It renews through the one renewal function of
// uc/certificate, which holds the renewal lock and adopts the child through
// the certificate store. The loop forces no reconnect after a renewal: the
// live stream keeps its connection, and the next dial loads the new key pair
// from disk.
//
// The loop writes one log line per attempt and the state file
// /run/runos/certificate-renewal.json.
package certrenew
