// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
)

// put uploads data as tok's holder through the CLI and returns the upload.
func (c *commanded) put(t *testing.T, tok, data string) types.BlobUpload {
	t.Helper()
	f := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(f, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errb := c.adc(tok, "blob", "put", "--file", f, "--json")
	var u types.BlobUpload
	if code != entry.ExitOK || contract.Validate("api/blob-upload", []byte(out)) != nil || json.Unmarshal([]byte(out), &u) != nil {
		t.Fatalf("put: %d %s %s", code, out, errb)
	}
	return u
}

func (c *commanded) purgeEvents(t *testing.T) int {
	return count(t, c.dir, `SELECT count(*) FROM events WHERE kind = '`+api.BlobPurged+`'`)
}

// P2's smoke against the daemon: an upload answers its digest and expiry;
// one over 1 MiB is refused (failed) with nothing stored; HEAD stores
// nothing; an operator lists the uploads with their actors, a supervisor
// may not; an expired upload is purged with one purge event.
func TestBlobUploadThroughTheCLI(t *testing.T) {
	c := runCommanded(t)
	op, sup := c.op.Reveal(), c.sup.Reveal()
	u := c.put(t, op, "hello")
	if u.Digest != "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" || u.Actor != c.opID {
		t.Fatalf("put: %+v", u)
	}
	big := filepath.Join(t.TempDir(), "big.bin")
	if os.WriteFile(big, make([]byte, 2<<20), 0o600) != nil {
		t.Fatal("writing big.bin")
	}
	if code, _, errb := c.adc(op, "blob", "put", "--file", big); code != entry.ExitRefused || !strings.Contains(errb, "failed/upload_bound_exceeded") {
		t.Fatalf("2 MiB: %d %s", code, errb)
	}
	if status, allow := c.request(t, http.MethodHead, op, "other"); status != http.StatusMethodNotAllowed || allow != "GET, POST" {
		t.Fatalf("HEAD: %d %q", status, allow)
	}
	if s := c.put(t, sup, "hello"); s.Actor != c.supID {
		t.Fatalf("supervisor put: %+v", s)
	}
	if code, _, errb := c.adc(sup, "dev", "blob", "uploads"); code != entry.ExitRefused || !strings.Contains(errb, "failed/operator_only") {
		t.Fatalf("supervisor list: %d %s", code, errb)
	}
	code, out, _ := c.adc(op, "dev", "blob", "uploads", "--json")
	var list types.ReadList
	if code != entry.ExitOK || json.Unmarshal([]byte(out), &list) != nil || len(list.Items) != 2 ||
		!strings.Contains(out, string(c.opID)) || !strings.Contains(out, string(c.supID)) || !strings.Contains(out, u.ExpiresAt) {
		t.Fatalf("list: %d %s", code, out)
	}
	version := `SELECT coalesce(max(seq), 0) FROM events WHERE subject = '` + string(c.opID) + `'`
	before := count(t, c.dir, version)
	later := time.Now().Add(blobstore.UploadTTL + time.Minute)
	if n, err := api.PurgeExpired(context.Background(), c.d.store, c.d.blobs, c.d.system, later, c.logs); n != 2 || err != nil {
		t.Fatalf("purged %d: %v", n, err)
	}
	if code, out, _ := c.adc(op, "dev", "blob", "uploads", "--json"); code != entry.ExitOK || !strings.Contains(out, `"items":[]`) || c.purgeEvents(t) != 2 {
		t.Fatalf("after the purge: %d %s; %d events", code, out, c.purgeEvents(t))
	}
	if after := count(t, c.dir, version); after != before {
		t.Fatalf("the purge moved the operator's version: %d to %d", before, after)
	}
	var purges []string // the purge events' frames, every byte of each
	for _, frame := range strings.Split(c.stream(t, op, api.BlobPurged), "\n\n") {
		if strings.Contains(frame, `"kind":"`+api.BlobPurged+`"`) {
			purges = append(purges, frame)
		}
	}
	for _, frame := range purges {
		if !strings.Contains(frame, string(u.Digest)) || strings.Contains(frame, string(c.opID)) || strings.Contains(frame, string(c.supID)) {
			t.Fatalf("a streamed purge event names its uploader: %s", frame)
		}
	}
	if len(purges) != 2 {
		t.Fatalf("%d purge events streamed", len(purges))
	}
}

// stream reads the event stream as tok's holder until it holds two whole events
// of kind, and returns what it read.
func (c *commanded) stream(t *testing.T, tok, kind string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://adcd"+api.EventsPath, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := unixClient(filepath.Join(c.dir, privateSocket)).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got []byte
	buf := make([]byte, 4096)
	complete := func() bool { // two frames of kind, the last one ended
		g := string(got)
		return strings.Count(g, kind) >= 2 && strings.Contains(g[strings.LastIndex(g, kind):], "\n\n")
	}
	for !complete() {
		n, err := resp.Body.Read(buf)
		if got = append(got, buf[:n]...); err != nil {
			t.Fatalf("the stream ended: %v: %s", err, got)
		}
	}
	return string(got)
}

// buryFailsFor fails the burial of one actor's uploads only.
type buryFailsFor struct {
	*blobstore.Store
	actor string
}

func (b buryFailsFor) Bury(u blobstore.Upload) error {
	if u.Actor == b.actor {
		return errors.New("injected: the bytes could not be removed")
	}
	return b.Store.Bury(u)
}

// One upload's failed purge neither stops the others nor goes unreported.
func TestPurgeGoesPastAFailure(t *testing.T) {
	c := runCommanded(t)
	c.put(t, c.op.Reveal(), "hello")
	c.put(t, c.sup.Reveal(), "hello")
	later := time.Now().Add(blobstore.UploadTTL + time.Minute)
	n, err := api.PurgeExpired(context.Background(), c.d.store, buryFailsFor{c.d.blobs, string(c.opID)}, c.d.system, later, c.logs)
	tombs, _ := c.d.blobs.Tombstones()
	if n != 1 || err == nil || !strings.Contains(err.Error(), string(c.opID)) || len(tombs) != 1 || tombs[0].Actor != string(c.opID) || c.purgeEvents(t) != 2 {
		t.Fatalf("purged %d: %v; tombstones %+v; %d events", n, err, tombs, c.purgeEvents(t))
	}
}

// request sends method to BlobsPath with tok's bearer and body.
func (c *commanded) request(t *testing.T, method, tok, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, "http://adcd"+api.BlobsPath, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := unixClient(filepath.Join(c.dir, privateSocket)).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	us, _ := c.d.blobs.Uploads()
	if method != http.MethodPost && len(us) != 1 {
		t.Fatalf("%s changed the uploads: %+v", method, us)
	}
	return resp.StatusCode, resp.Header.Get("Allow")
}

// buryFails is the blob store with burial failing, as a crash after the
// purge event committed would leave it.
type buryFails struct{ *blobstore.Store }

func (buryFails) Bury(blobstore.Upload) error {
	return errors.New("injected: the bytes could not be removed")
}

// The purge event is committed before the bytes go: when removal fails,
// the event stands and the bytes stay set aside, and the next purge (as
// at start) finishes it by replaying the same command, adding no event.
// A malformed entry is skipped and logged, never stopping the purge or
// the list.
func TestPurgeEventBeforeRemoval(t *testing.T) {
	c := runCommanded(t)
	op := c.op.Reveal()
	u := c.put(t, op, "hello")
	junk := filepath.Join(c.dir, "blobs", "uploads", string(c.opID), "junk")
	if os.WriteFile(junk, nil, 0o600) != nil {
		t.Fatal("planting junk")
	}
	if code, out, _ := c.adc(op, "dev", "blob", "uploads", "--json"); code != entry.ExitOK || !strings.Contains(out, string(u.Digest)) {
		t.Fatalf("list beside a malformed entry: %d %s", code, out)
	}
	later := time.Now().Add(blobstore.UploadTTL + time.Minute)
	if n, err := api.PurgeExpired(context.Background(), c.d.store, buryFails{c.d.blobs}, c.d.system, later, c.logs); n != 0 || err == nil {
		t.Fatalf("purged %d: %v", n, err)
	}
	tomb := filepath.Join(c.dir, "blobs", "uploads", string(c.opID), ".purging-"+strings.TrimPrefix(string(u.Digest), "sha256:"))
	if _, err := os.Stat(tomb); err != nil || c.purgeEvents(t) != 1 {
		t.Fatalf("after a failed removal: the bytes %v, %d events", err, c.purgeEvents(t))
	}
	if n, err := api.PurgeExpired(context.Background(), c.d.store, c.d.blobs, c.d.system, later, c.logs); n != 1 || err != nil {
		t.Fatalf("finishing: purged %d: %v", n, err)
	}
	if _, err := os.Stat(tomb); !os.IsNotExist(err) || c.purgeEvents(t) != 1 {
		t.Fatalf("after finishing: the bytes %v, %d events", err, c.purgeEvents(t))
	}
	if !strings.Contains(c.logs.String(), "adcd: uploads: skipped:") || !strings.Contains(c.logs.String(), "junk") {
		t.Fatalf("the malformed entry was not logged: %s", c.logs.String())
	}
}
