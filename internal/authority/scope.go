// SPDX-License-Identifier: Apache-2.0

package authority

import "errors"

// ErrInvalidScope: a command's scope does not satisfy its action's scope
// schema or input form. A handler's Derive wraps it, so the request layer
// answers the request as malformed (nothing decided), never as a fault.
var ErrInvalidScope = errors.New("authority: the scope is not valid for the action")
