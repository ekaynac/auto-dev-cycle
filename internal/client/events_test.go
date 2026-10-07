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
	for _, ctype := range []string{"application/json", "text/event-streamx", "text/event-stream-not", ""} {
		if _, err := answering(t, http.StatusOK, ctype, "{}", &path, &auth).Follow(context.Background(), 0); err == nil {
			t.Errorf("%q was taken for a stream", ctype)
		}
	}
	body, err = answering(t, http.StatusOK, "text/event-stream; charset=utf-8", "", &path, &auth).Follow(context.Background(), 0)
	if err != nil {
		t.Fatalf("with a parameter: %v", err)
	}
	body.Close()
	var r *Refused
	if _, err := answering(t, http.StatusServiceUnavailable, "application/json", `{"outcome":"could_not_run","reason":"store_unreadable","detail":"d"}`, &path, &auth).
		Follow(context.Background(), 0); !errors.As(err, &r) || r.Result.Outcome != vocab.CouldNotRun {
		t.Errorf("unreadable: %v", err)
	}
}
