// SPDX-License-Identifier: Apache-2.0

// Package identity keeps actors and their bearer tokens (docs/design/m0/04
// "Actors and tokens"). A bearer token is tok_<id>.<secret>: the secret is
// shown once, at issue, and only its argon2id hash is kept, so the id finds
// the token record and the hash verifies the secret.
package identity
