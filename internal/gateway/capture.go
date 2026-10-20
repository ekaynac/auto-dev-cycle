// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
)

// CaptureTTL is a capture's finite retention (00-slice-plan §8 item 4: P7D).
const CaptureTTL = 7 * 24 * time.Hour

// ClassRestricted is a capture's blob class: kept from every builder and
// reviewer, read only by the exit checks.
const ClassRestricted = "restricted"

// Capture keeps the request bodies the gateway sends, for an exit run's
// checks (an acceptance canary in no request). Only exit runs configure
// one (§8 item 4); the body it gets is the one sent, before the credential
// is injected, and no header.
type Capture interface {
	Keep(ctx context.Context, b Binding, body []byte) error
}

// Captured is one kept body: its blob, class and expiry.
type Captured struct {
	Lease     types.LeaseID
	Attempt   types.AttemptID
	Call      types.CallID
	Digest    string
	Class     string
	ExpiresAt time.Time
}

// BlobCapture keeps each body as a restricted blob in Blobs (adcd's
// capture store, a blob store of its own), and hands its record to Record:
// adcd records it (blob.capture_kept) and purges it at its expiry, which an
// exit run's record lists.
type BlobCapture struct {
	Blobs  *blobstore.Store
	Now    func() time.Time
	Record func(context.Context, Captured) error
}

func (c BlobCapture) Keep(ctx context.Context, b Binding, body []byte) error {
	if c.Blobs == nil || c.Now == nil || c.Record == nil {
		return errors.New("gateway: the capture is not configured")
	}
	digest, err := c.Blobs.Put(bytes.NewReader(body), "")
	if err != nil {
		return err
	}
	return c.Record(ctx, Captured{Lease: b.Lease, Attempt: b.Attempt, Call: b.Call, Digest: digest, Class: ClassRestricted,
		ExpiresAt: c.Now().Add(CaptureTTL).UTC()})
}
