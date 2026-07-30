//go:build slatedb

package storage_test

// A real 2-node shale cluster, in-process on live MinIO, proving a ring
// rebalance is lossless for hostthis's data shapes: pastes and their versions,
// the scan-derived per-owner quota, and the identity_pastes projection backing
// ListByOwner. Lossless means every paste stays readable through EITHER node
// (forwarding when the local one is not the owner), the quota sum survives its
// move to whichever node owns the {id} shard, and the projection survives so
// ListByOwner stays correct.
//
//	CGO_ENABLED=1 \
//	CGO_LDFLAGS="-L<slatedb-lib-dir>" DYLD_LIBRARY_PATH="<slatedb-lib-dir>" \
//	MINIO_TEST_ENDPOINT=http://localhost:9000 \
//	MINIO_TEST_METADATA_BUCKET=hostthis-metadata \
//	MINIO_TEST_ACCESS_KEY=admin MINIO_TEST_SECRET_KEY=supersecret \
//	go test -tags slatedb -run TestShaleRebalance ./internal/storage
//
// NewShaleRepo binds the peer-forwarding listener and advertises the ACTUAL
// bound address (cluster.Open advertises a GRPCAddr but does not serve it), so
// the read-via-non-owner assertions exercise the production forwarding path.
//
// A rebalance that moved NOTHING would satisfy every read assertion trivially,
// so one assertion requires node B to hold units.

import (
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// rebalNode bundles one in-process node: its ShaleRepo plus the addresses peers
// seed off and forward to. The repo owns its peer-forwarding gRPC server, so
// repo.Close() is the only teardown needed.
type rebalNode struct {
	id       string
	repo     *storage.ShaleRepo
	bindAddr string
	grpcAddr string
}

func (n *rebalNode) close() {
	if n.repo != nil {
		_ = n.repo.Close() // also gracefully stops the repo's gRPC server
	}
}

// freeTCPPort grabs an OS-assigned ephemeral port and releases it for the
// caller to bind. The release->rebind race is harmless on loopback.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeTCPPort: %v", err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return p
}

func loopback(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }

// startRebalNode opens a multi-node ShaleRepo. seedBind="" makes this the
// founding node; a non-empty one joins the existing ring. R=1 is the
// horizontal-write-scaling shape the gate is about. GRPCAddr "127.0.0.1:0"
// takes an OS-assigned port, read back via repo.GRPCAddr() for a sibling to
// reference.
func startRebalNode(t *testing.T, id, dbName, seedBind string) *rebalNode {
	t.Helper()
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	bucket := envOrDefault("MINIO_TEST_METADATA_BUCKET", "hostthis-metadata")
	access := envOrDefault("MINIO_TEST_ACCESS_KEY", "admin")
	secret := envOrDefault("MINIO_TEST_SECRET_KEY", "supersecret")

	bindAddr := loopback(freeTCPPort(t))

	var seeds []string
	if seedBind != "" {
		seeds = []string{seedBind}
	}

	repo, err := storage.NewShaleRepo(storage.ShaleConfig{
		NodeID:            id,
		Endpoint:          endpoint,
		Region:            "us-east-1",
		Bucket:            bucket,
		AccessKey:         access,
		SecretKey:         secret,
		UseSSL:            false,
		DbName:            dbName,
		BindAddr:          bindAddr,
		GRPCAddr:          "127.0.0.1:0", // OS-assigned; the repo serves the actual port
		Seeds:             seeds,
		ReplicationFactor: 1,
		// Multi-node REQUIRES sharding: a bind address with UnitCount 0 is
		// refused at cluster.Open.
		UnitCount: 4,
	})
	if err != nil {
		t.Fatalf("node %s: NewShaleRepo: %v", id, err)
	}

	n := &rebalNode{
		id:       id,
		repo:     repo,
		bindAddr: bindAddr,
		grpcAddr: repo.GRPCAddr(), // ACTUAL bound forwarding addr a peer would use
	}
	t.Cleanup(n.close)
	return n
}

// waitMembers polls every node's cluster until each reports `want` ring
// members or the deadline expires. SWIM convergence is not synchronous,
// so any routed multi-node op must wait for the ring to agree first.
func waitMembers(t *testing.T, nodes []*rebalNode, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ok := true
		for _, n := range nodes {
			if len(n.repo.ClusterForTest().Members()) != want {
				ok = false
				break
			}
		}
		if ok {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	sizes := make([]int, len(nodes))
	for i, n := range nodes {
		sizes[i] = len(n.repo.ClusterForTest().Members())
	}
	t.Fatalf("ring did not converge to %d members within %s: sizes=%v", want, timeout, sizes)
}

// pasteFor builds a v1 paste at now with the standard retention window. Local
// to this file so the gate owns its fixtures.
func pasteFor(slug, identity, name string, size int, now time.Time) domain.Paste {
	return domain.Paste{
		Slug:          domain.Slug(slug),
		Identity:      domain.Identity(identity),
		Kind:          domain.KindHTML,
		ContentSHA:    "sha-" + slug + "-v1",
		Size:          size,
		Name:          name,
		PinnedVersion: 0,
		CreatedAt:     now,
		UpdatedAt:     now,
		ExpiresAt:     now.Add(domain.DefaultRetentionWindow),
	}
}

func TestShaleRebalance_TwoNodeLossless(t *testing.T) {
	if os.Getenv("MINIO_TEST_ENDPOINT") == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping 2-node rebalance gate (start dev MinIO first)")
	}

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	epoch := time.Now().UnixNano()

	// --- node A: the seed/founder ---
	// One dbName SHARED by both nodes: DbName is the key-PREFIX every unit
	// lives beneath in object storage, so distinct names point the nodes at
	// disjoint prefix trees, B opens empty units, and A's data is invisible -
	// a spurious ASSERTION 1 data-loss failure with nothing actually lost.
	dbName := fmt.Sprintf("rebal-%d", epoch)
	nodeA := startRebalNode(t, "rebal-A", dbName, "")

	// --- the dataset, written through node A's PUBLIC api ---
	//
	// Three identities with varied paste shapes so the projections + quota sums
	// are non-trivial. Slugs use only SlugAlphabet chars (no l/o/0/1); sizes are
	// distinct so a miscount is obvious in the failure message.
	type want struct {
		identity   string
		activeByte int            // expected SumActiveBytesByOwner
		slugs      map[string]int // slug -> latest active version number
	}

	cap0 := int64(0) // no caps; the quota sum still tracks bytes exactly

	// alice
	aliceP1 := pasteFor("aaaa2345", "key:alice", "alice one", 100, now)
	aliceP2 := pasteFor("aaab2345", "key:alice", "alice two", 250, now)
	mustInsert(t, nodeA.repo, aliceP1, cap0)
	mustInsert(t, nodeA.repo, aliceP2, cap0)

	// bob, paste 1: v1 (300) then append v2 (400) then append v3 (500).
	// Latest active version = 3; active bytes contributed = 300+400+500.
	bobMulti := pasteFor("bbbb2345", "key:bob", "bob multi", 300, now)
	mustInsert(t, nodeA.repo, bobMulti, cap0)
	mustAppend(t, nodeA.repo, "bbbb2345", domain.KindHTML, "sha-bbbb2345-v2", 400, cap0, now.Add(time.Minute))
	mustAppend(t, nodeA.repo, "bbbb2345", domain.KindHTML, "sha-bbbb2345-v3", 500, cap0, now.Add(2*time.Minute))

	// bob, paste 2: v1 (700), append v2 (200), then TOMBSTONE v1. A tombstoned
	// version sheds its bytes from the quota sum but stays listable (flagged
	// deleted). Latest active = 2.
	bobTomb := pasteFor("bbbc2345", "key:bob", "bob tomb", 700, now)
	mustInsert(t, nodeA.repo, bobTomb, cap0)
	mustAppend(t, nodeA.repo, "bbbc2345", domain.KindHTML, "sha-bbbc2345-v2", 200, cap0, now.Add(time.Minute))
	if err := nodeA.repo.DeleteVersion("bbbc2345", 1); err != nil {
		t.Fatalf("DeleteVersion bbbc2345 v1: %v", err)
	}

	// carol
	carolP1 := pasteFor("cccc2345", "key:carol", "carol one", 900, now)
	mustInsert(t, nodeA.repo, carolP1, cap0)

	wants := []want{
		{
			identity:   "key:alice",
			activeByte: 100 + 250,
			slugs:      map[string]int{"aaaa2345": 1, "aaab2345": 1},
		},
		{
			identity:   "key:bob",
			activeByte: (300 + 400 + 500) + (700 + 200) - 700, // tombstoned v1 shed
			slugs:      map[string]int{"bbbb2345": 3, "bbbc2345": 2},
		},
		{
			identity:   "key:carol",
			activeByte: 900,
			slugs:      map[string]int{"cccc2345": 1},
		},
	}

	allSlugs := []string{"aaaa2345", "aaab2345", "bbbb2345", "bbbc2345", "cccc2345"}

	// --- record the BEFORE state through node A (single node, no routing) ---
	beforeBytes := map[string]int{}
	for _, w := range wants {
		got, err := nodeA.repo.SumActiveBytesByOwner(w.identity, now)
		if err != nil {
			t.Fatalf("before: SumActiveBytesByOwner(%s): %v", w.identity, err)
		}
		if got != w.activeByte {
			t.Fatalf("before: %s active bytes = %d, want %d (dataset/quota math is wrong even single-node)",
				w.identity, got, w.activeByte)
		}
		beforeBytes[w.identity] = got
		// Every paste readable single-node before the join.
		for slug := range w.slugs {
			if _, err := nodeA.repo.Get(domain.Slug(slug)); err != nil {
				t.Fatalf("before: Get(%s) via A: %v", slug, err)
			}
		}
	}
	t.Logf("BEFORE (single node A): per-owner active bytes = %v", beforeBytes)

	keysBeforeA, err := nodeA.repo.LocalKeyCountForTest(nil)
	if err != nil {
		t.Fatalf("before: local key count A: %v", err)
	}
	t.Logf("BEFORE: node A holds %d local keys; node B not yet joined", keysBeforeA)

	// --- node B joins -> 2-node ring -> rebalance ---
	nodeB := startRebalNode(t, "rebal-B", dbName, nodeA.bindAddr)
	nodes := []*rebalNode{nodeA, nodeB}
	waitMembers(t, nodes, 2, 15*time.Second)
	t.Logf("ring converged to 2 members")

	// The cluster's RebalanceSettleDelay is 5s and ShaleConfig exposes no knob
	// to shrink it, so sleep past the debounce first: WaitForIdle inside that
	// window reports idle immediately because nothing is pending yet. The idle
	// budget then covers the slate-backed streaming handoff.
	time.Sleep(6 * time.Second)
	idleCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, n := range nodes {
		if err := n.repo.WaitForRebalanceIdleForTest(idleCtx); err != nil {
			t.Fatalf("rebalance did not reach idle on %s: %v", n.id, err)
		}
	}
	// Drain window so the source-side grace sweep ticks land before physical
	// placement is inspected.
	time.Sleep(1 * time.Second)
	t.Logf("rebalance reported idle on both nodes")

	// --- ASSERTION 4 (run first, it shapes the others): units redistributed.
	// A join moves a unit's LEASE, not its bytes (a unit is a database at a
	// fixed prefix in shared object storage), so mounted-unit counts are the
	// instrument here and local key counts would read 0 on both nodes. Zero
	// mounted units on B means the rebalance moved nothing, which makes the
	// read assertions pass trivially while hiding a broken handoff.
	readyA := nodeA.repo.MountReadiness()
	readyB := nodeB.repo.MountReadiness()
	keysA, keysB := readyA.MountedUnits, readyB.MountedUnits
	t.Logf("AFTER: mounted-unit distribution: A=%d B=%d (total=%d)", keysA, keysB, keysA+keysB)
	if keysB == 0 {
		t.Fatalf("ASSERTION 4 FAILED: node B has mounted ZERO units after the join; the ring "+
			"delivered no units to the new node (A=%d B=%d). Rebalance is a no-op, not lossless.", keysA, keysB)
	}
	if keysA == 0 {
		t.Fatalf("ASSERTION 4 FAILED: node A has mounted ZERO units after the join; everything moved off the "+
			"seed (A=%d B=%d), which is implausible for a 2-node ring and signals a placement bug.", keysA, keysB)
	}

	// --- diagnostic dump: per paste key, the ring's owner vs where the bytes
	// physically live.
	for _, slug := range allSlugs {
		pasteKey := []byte("pastes/" + slug)
		t.Logf("DIAG %s: ring-owner A?=%v B?=%v | physically-on A=%v B=%v",
			slug,
			nodeA.repo.OwnsKeyForTest(pasteKey),
			nodeB.repo.OwnsKeyForTest(pasteKey),
			nodeA.repo.LocalGetPresentForTest(pasteKey),
			nodeB.repo.LocalGetPresentForTest(pasteKey),
		)
	}

	// --- ASSERTION 1: every paste readable via BOTH nodes' repos. Whichever
	// node does NOT own a given {slug} shard forwards the Get over gRPC and
	// must return identical content.
	for _, slug := range allSlugs {
		ds := domain.Slug(slug)
		pasteKey := []byte("pastes/" + slug)
		gotA, errA := nodeA.repo.Get(ds)
		gotB, errB := nodeB.repo.Get(ds)
		if errA != nil {
			t.Fatalf("ASSERTION 1 FAILED (DATA LOSS): Get(%s) via node A after rebalance: %v\n"+
				"  ring-owner: A?=%v B?=%v | physically-on: A=%v B=%v\n"+
				"  This is the founder-grows (1->2) rebalance bug: the ring advanced ownership of this\n"+
				"  partition to the new node, but the partition's keys were NEVER streamed there. The bytes\n"+
				"  physically survive on the old node (LOST FROM THE CLUSTER, not destroyed), so a routed Get\n"+
				"  goes to the new owner and finds nothing. The prod multi-node reshape MUST NOT ship until\n"+
				"  shale's bootstrap handoff moves every newly-owned partition (note: shale change, do not push).",
				slug, errA,
				nodeA.repo.OwnsKeyForTest(pasteKey), nodeB.repo.OwnsKeyForTest(pasteKey),
				nodeA.repo.LocalGetPresentForTest(pasteKey), nodeB.repo.LocalGetPresentForTest(pasteKey))
		}
		if errB != nil {
			t.Fatalf("ASSERTION 1 FAILED (DATA LOSS): Get(%s) via node B (forwarded) after rebalance: %v\n"+
				"  ring-owner: A?=%v B?=%v | physically-on: A=%v B=%v",
				slug, errB,
				nodeA.repo.OwnsKeyForTest(pasteKey), nodeB.repo.OwnsKeyForTest(pasteKey),
				nodeA.repo.LocalGetPresentForTest(pasteKey), nodeB.repo.LocalGetPresentForTest(pasteKey))
		}
		if gotA.Slug != ds || gotB.Slug != ds {
			t.Fatalf("ASSERTION 1 FAILED: Get(%s) returned wrong slug: A=%q B=%q", slug, gotA.Slug, gotB.Slug)
		}
		if gotA.ContentSHA != gotB.ContentSHA || gotA.Size != gotB.Size || gotA.Name != gotB.Name {
			t.Fatalf("ASSERTION 1 FAILED: Get(%s) diverged between owner + forwarder:\n  A=%+v\n  B=%+v", slug, gotA, gotB)
		}
		// Versions survived too: list via both, assert the same set.
		va, errVA := nodeA.repo.ListVersions(ds)
		vb, errVB := nodeB.repo.ListVersions(ds)
		if errVA != nil || errVB != nil {
			t.Fatalf("ASSERTION 1 FAILED: ListVersions(%s): A=%v B=%v", slug, errVA, errVB)
		}
		if len(va) != len(vb) {
			t.Fatalf("ASSERTION 1 FAILED: ListVersions(%s) length differs across nodes: A=%d B=%d", slug, len(va), len(vb))
		}
	}
	t.Logf("ASSERTION 1 PASSED: all %d pastes (+ their version sets) readable via BOTH nodes; forwarding works", len(allSlugs))

	// --- ASSERTION 2: the per-owner quota survived the move. The {id} shard
	// and the identity_pastes entries it sums may now live on B, so the read
	// forwards; the sum must still equal the BEFORE value exactly via either
	// node.
	for _, w := range wants {
		gotA, errA := nodeA.repo.SumActiveBytesByOwner(w.identity, now)
		gotB, errB := nodeB.repo.SumActiveBytesByOwner(w.identity, now)
		if errA != nil || errB != nil {
			t.Fatalf("ASSERTION 2 FAILED: SumActiveBytesByOwner(%s): A=%v B=%v", w.identity, errA, errB)
		}
		if gotA != w.activeByte {
			t.Fatalf("ASSERTION 2 FAILED (QUOTA MISCOUNT): %s active bytes via A = %d, want %d (before=%d). "+
				"The per-owner quota sum did not survive the rebalance.", w.identity, gotA, w.activeByte, beforeBytes[w.identity])
		}
		if gotB != w.activeByte {
			t.Fatalf("ASSERTION 2 FAILED (QUOTA MISCOUNT): %s active bytes via B (forwarded) = %d, want %d. "+
				"The quota sum is wrong when read through the non-owning node.", w.identity, gotB, w.activeByte)
		}
	}
	t.Logf("ASSERTION 2 PASSED: per-owner quota sums survived exactly via both nodes: %v", beforeBytes)

	// --- ASSERTION 3: projections survived -> ListByOwner correct via both
	// nodes. Same paste set, same latest-active-version per slug.
	for _, w := range wants {
		assertListByOwner(t, nodeA.repo, "A", w.identity, w.slugs)
		assertListByOwner(t, nodeB.repo, "B", w.identity, w.slugs)
	}
	t.Logf("ASSERTION 3 PASSED: ListByOwner returns the correct paste set + latest versions for every owner via both nodes")

	t.Logf("GATE PASSED: 2-node ring rebalance is LOSSLESS for pastes + quota + projections")
}

// --- helpers ---------------------------------------------------------------

func mustInsert(t *testing.T, repo *storage.ShaleRepo, p domain.Paste, cap int64) {
	t.Helper()
	if err := repo.InsertWithQuotaCheck(context.Background(), p, cap, p.CreatedAt); err != nil {
		t.Fatalf("InsertWithQuotaCheck(%s): %v", p.Slug, err)
	}
}

func mustAppend(t *testing.T, repo *storage.ShaleRepo, slug string, kind domain.ContentKind, sha string, size int, cap int64, now time.Time) {
	t.Helper()
	if _, err := repo.AppendVersionWithQuotaCheck(context.Background(), domain.Slug(slug), kind, sha, size, cap, now); err != nil {
		t.Fatalf("AppendVersionWithQuotaCheck(%s): %v", slug, err)
	}
}

// assertListByOwner checks ListByOwner via the given repo returns exactly
// the wanted slug set, each at its wanted latest-active version.
func assertListByOwner(t *testing.T, repo *storage.ShaleRepo, label, owner string, wantSlugs map[string]int) {
	t.Helper()
	got, err := repo.ListByOwner(owner)
	if err != nil {
		t.Fatalf("ASSERTION 3 FAILED: ListByOwner(%s) via %s: %v", owner, label, err)
	}
	gotSlugs := map[string]int{}
	for _, p := range got {
		gotSlugs[p.Slug.String()] = p.LatestVersion
	}
	if len(gotSlugs) != len(wantSlugs) {
		var names []string
		for s := range gotSlugs {
			names = append(names, s)
		}
		sort.Strings(names)
		t.Fatalf("ASSERTION 3 FAILED (PROJECTION LOST): ListByOwner(%s) via %s returned %d pastes %v, want %d %v. "+
			"The identity_pastes projection did not survive the rebalance.", owner, label, len(gotSlugs), names, len(wantSlugs), keysOf(wantSlugs))
	}
	for slug, wantVer := range wantSlugs {
		gotVer, ok := gotSlugs[slug]
		if !ok {
			t.Fatalf("ASSERTION 3 FAILED (PROJECTION LOST): ListByOwner(%s) via %s is missing paste %s", owner, label, slug)
		}
		if gotVer != wantVer {
			t.Fatalf("ASSERTION 3 FAILED: ListByOwner(%s) via %s: paste %s latest version = %d, want %d",
				owner, label, slug, gotVer, wantVer)
		}
	}
}

func keysOf(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
