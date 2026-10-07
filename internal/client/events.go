// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/internal/api"
)

// Follow opens the event stream after seq and returns its body, which the
// caller reads as Server-Sent Events and closes.
func (t Transport) Follow(ctx context.Context, after int64) (io.ReadCloser, error) {
	resp, err := t.do(ctx, http.MethodGet, api.EventsPath+"?after="+strconv.FormatInt(after, 10), nil, true)
	if err := refusal(resp, err); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		resp.Body.Close()
		return nil, errors.New("the controller's answer is not an event stream")
	}
	return resp.Body, nil
}
