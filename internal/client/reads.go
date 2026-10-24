// SPDX-License-Identifier: Apache-2.0

package client

// Reads and the event stream (docs/design/m0/08 "Reads, commands,
// events"), sent like commands on a verified connection. A read's body is
// returned only once it validates against its resource's schemas; a
// refusal is returned only as a registered result.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Whoami is the caller's own read, outside the resources.
const Whoami = "whoami"

// maxRefusalBytes bounds a refusal's body.
const maxRefusalBytes = 64 << 10

// ErrNoResource: the controller serves no such resource.
var ErrNoResource = errors.New("the controller serves no such resource")

// ErrInvalidRead: the read's body does not validate; it is never quoted.
var ErrInvalidRead = errors.New("the controller's read response does not validate against its schema")

// Refused is a read or stream the controller refused with a registered
// result (failed or could_not_run).
type Refused struct{ Result vocab.Result }

// Error names only the registered outcome and reason, never the
// controller's free-text detail, which a caller shows escaped if at all.
func (r *Refused) Error() string {
	return fmt.Sprintf("the controller refused: %s/%s", r.Result.Outcome, r.Result.Reason)
}

// DecodeResult reads a refusal or a stream's closing result: strict, a
// registered reason its outcome allows, and only failed or could_not_run.
func DecodeResult(raw []byte) (vocab.Result, error) {
	var r vocab.Result
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	reg, err := vocab.Load()
	if err == nil {
		err = reg.CheckRead(r)
	}
	if err == nil && r.Outcome != vocab.Failed && r.Outcome != vocab.CouldNotRun {
		err = fmt.Errorf("a refusal with outcome %q", r.Outcome)
	}
	return r, err
}

// Read gets /v1/whoami or a registered resource; the body is returned once
// it validates: the read list, its resource, and every record against the
// resource's own schema.
func (t Transport) Read(ctx context.Context, resource string) ([]byte, error) {
	schema := "api/whoami"
	res, ok := api.Registered()[resource]
	if ok {
		schema = strings.TrimSuffix(res.Schema, ".json")
	} else if resource != Whoami {
		return nil, ErrNoResource
	}
	resp, err := t.do(ctx, http.MethodGet, "/v1/"+resource, nil, false)
	if err := refusal(resp, err); err != nil {
		return nil, err
	}
	return answer(resp, nil, "a read", func(raw []byte) error {
		if !ok {
			return orInvalid(contract.Validate(schema, raw))
		}
		return validList(raw, resource, schema)
	})
}

// ReadTask gets one task's read (api.TaskPath): the tasks list holding
// that task's record alone, or none; returned once it validates as Read's
// tasks list does, with at most that one record (P25; Opus #430 H1).
func (t Transport) ReadTask(ctx context.Context, task string) ([]byte, error) {
	const resource = "tasks"
	res, ok := api.Registered()[resource]
	if !ok {
		return nil, ErrNoResource
	}
	resp, err := t.do(ctx, http.MethodGet, "/v1/tasks/"+url.PathEscape(task), nil, false)
	if err := refusal(resp, err); err != nil {
		return nil, err
	}
	return answer(resp, nil, "a task read", func(raw []byte) error {
		var list struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		if err := validList(raw, resource, strings.TrimSuffix(res.Schema, ".json")); err != nil {
			return err
		}
		if json.Unmarshal(raw, &list) != nil || len(list.Items) > 1 || len(list.Items) == 1 && list.Items[0].ID != task {
			return ErrInvalidRead
		}
		return nil
	})
}

// validList checks a read list of resource, every item against schema.
func validList(raw []byte, resource, schema string) error {
	var list struct {
		Resource string            `json:"resource"`
		Items    []json.RawMessage `json:"items"`
	}
	if contract.Validate("api/read-list", raw) != nil || json.Unmarshal(raw, &list) != nil || list.Resource != resource {
		return ErrInvalidRead
	}
	for _, item := range list.Items {
		if contract.Validate(schema, item) != nil {
			return ErrInvalidRead
		}
	}
	return nil
}

// Blob uploads data to /v1/blobs and returns the upload, or with nil data
// lists the uploads (operator only); either body is returned once it
// validates, and a refusal only as a registered result.
func (t Transport) Blob(ctx context.Context, data []byte) ([]byte, error) {
	method, body := http.MethodGet, io.Reader(nil)
	if data != nil {
		method, body = http.MethodPost, bytes.NewReader(data)
	}
	resp, err := t.do(ctx, method, api.BlobsPath, body, false)
	if err := refusal(resp, err); err != nil {
		return nil, err
	}
	return answer(resp, nil, "a blob response", func(raw []byte) error {
		if data != nil {
			return orInvalid(contract.Validate("api/blob-upload", raw))
		}
		return validList(raw, api.BlobUploads, "api/blob-upload")
	})
}

// SandboxProbe runs the sandbox probe (operator only) on image in mode
// and returns the report once it validates.
func (t Transport) SandboxProbe(ctx context.Context, image, mode string) ([]byte, error) {
	q := url.Values{"image": {image}, "mode": {mode}}
	resp, err := t.do(ctx, http.MethodPost, api.SandboxProbePath+"?"+q.Encode(), nil, false)
	if err := refusal(resp, err); err != nil {
		return nil, err
	}
	return answer(resp, nil, "a sandbox probe", func(raw []byte) error { return orInvalid(contract.Validate("api/sandbox-probe", raw)) })
}

func orInvalid(err error) error {
	if err != nil {
		return ErrInvalidRead
	}
	return nil
}

// refusal is nil for a 200 (its body left open), else why, the body closed.
func refusal(resp *http.Response, err error) error {
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return ErrUnauthenticated
	case http.StatusBadRequest:
		return ErrBadRequest
	case http.StatusNotFound:
		return ErrNoResource
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxRefusalBytes))
	if r, err := DecodeResult(raw); err == nil {
		return &Refused{Result: r}
	}
	// Fixed text: an unregistered body is never quoted.
	return fmt.Errorf("the controller answered HTTP %d without a registered result", resp.StatusCode)
}
