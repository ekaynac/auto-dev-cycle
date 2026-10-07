// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The stream is opened after a seq, and only an event stream is returned.
func TestFollow(t *testing.T) {
	var path, auth string
	body, err := answering(t, http.StatusOK, "text/event-stream", "id: 1\n\n", &path, &auth).Follow(context.Background(), 41)
	if err != nil || path != "/v1/events?after=41" {
		t.Fatalf("%v %s", err, path)
	}
	body.Close()
	if _, err := answering(t, http.StatusOK, "application/json", "{}", &path, &auth).Follow(context.Background(), 0); err == nil {
		t.Error("a JSON answer was taken for a stream")
	}
	var r *Refused
	if _, err := answering(t, http.StatusServiceUnavailable, "application/json", `{"outcome":"could_not_run","reason":"store_unreadable","detail":"d"}`, &path, &auth).
		Follow(context.Background(), 0); !errors.As(err, &r) || r.Result.Outcome != vocab.CouldNotRun {
		t.Errorf("unreadable: %v", err)
	}
}
