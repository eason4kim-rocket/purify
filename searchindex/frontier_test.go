package searchindex

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestFrontierLeaseOneRootAtATimeAndCompletes(t *testing.T) {
	store := openTestStore(t)
	now := time.Unix(1_700_000_100, 0)
	if _, err := store.Enqueue(context.Background(), []FrontierItem{
		{URL: "https://a.example/1", Root: "example"},
		{URL: "https://a.example/2", Root: "example"},
		{URL: "https://b.example/1", Root: "b.example"},
	}); err != nil {
		t.Fatal(err)
	}
	first, ok, err := store.Lease(context.Background(), now)
	if err != nil || !ok {
		t.Fatalf("first Lease() = %#v, %v, %v", first, ok, err)
	}
	second, ok, err := store.Lease(context.Background(), now)
	if err != nil || !ok {
		t.Fatalf("second Lease() = %#v, %v, %v", second, ok, err)
	}
	if first.Root == second.Root {
		t.Fatalf("leased the same root twice: %#v %#v", first, second)
	}
	third, ok, err := store.Lease(context.Background(), now)
	if err != nil || ok {
		t.Fatalf("third Lease() = %#v, %v, %v want empty", third, ok, err)
	}
	if err := store.Complete(context.Background(), first.URL); err != nil {
		t.Fatal(err)
	}
	again, ok, err := store.Lease(context.Background(), now.Add(time.Second))
	if err != nil || !ok || again.Root != first.Root {
		t.Fatalf("lease after complete = %#v, %v, %v", again, ok, err)
	}
}

// TestFrontierReleaseStaleLeasesUnblocksTheRoot locks crash recovery: a killed
// crawler leaves rows in "leased" forever, and because Lease refuses any root
// with a leased row, a handful of stale leases can block every pending URL of
// the slow-host tail and make a full frontier look drained.
func TestFrontierReleaseStaleLeasesUnblocksTheRoot(t *testing.T) {
	store := openTestStore(t)
	now := time.Unix(1_700_000_100, 0)
	if _, err := store.Enqueue(context.Background(), []FrontierItem{
		{URL: "https://a.example/1", Root: "example"},
		{URL: "https://a.example/2", Root: "example"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Lease(context.Background(), now); err != nil || !ok {
		t.Fatalf("Lease() = %v, %v", ok, err)
	}
	// The crawler dies here; its lease survives in the table.
	if _, ok, err := store.Lease(context.Background(), now); err != nil || ok {
		t.Fatalf("blocked root leased anyway: %v, %v", ok, err)
	}
	released, err := store.ReleaseStaleLeases(context.Background())
	if err != nil || released != 1 {
		t.Fatalf("ReleaseStaleLeases() = %d, %v", released, err)
	}
	item, ok, err := store.Lease(context.Background(), now)
	if err != nil || !ok {
		t.Fatalf("Lease() after release = %v, %v", ok, err)
	}
	if item.Root != "example" {
		t.Fatalf("released root not leasable: %#v", item)
	}
}

// TestEnqueueBoundedStopsAtThePerRootBudget locks the growth guardrail: link
// discovery feeds the frontier while the crawl runs, so without a per-root
// budget one heavily linked host could flood the table and starve every other
// root of crawl time.
func TestEnqueueBoundedStopsAtThePerRootBudget(t *testing.T) {
	store := openTestStore(t)
	seeded, err := store.Enqueue(context.Background(), []FrontierItem{
		{URL: "https://a.example/1", Root: "example"},
		{URL: "https://a.example/2", Root: "example"},
	})
	if err != nil || seeded != 2 {
		t.Fatalf("Enqueue() = %d, %v", seeded, err)
	}
	inserted, err := store.EnqueueBounded(context.Background(), []FrontierItem{
		{URL: "https://a.example/2", Root: "example"}, // duplicate: must not consume budget
		{URL: "https://a.example/3", Root: "example"},
		{URL: "https://a.example/4", Root: "example"}, // over budget
		{URL: "https://b.example/1", Root: "b.example"},
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if inserted != 2 {
		t.Fatalf("EnqueueBounded() inserted = %d, want 2 (one per root)", inserted)
	}
	for url, want := range map[string]bool{
		"https://a.example/3": true,
		"https://a.example/4": false,
		"https://b.example/1": true,
	} {
		if got := frontierHas(t, store, url); got != want {
			t.Fatalf("frontier row %s present = %v, want %v", url, got, want)
		}
	}
	// Completed rows are spent budget, so they still count against the cap.
	item, ok, err := store.Lease(context.Background(), time.Unix(1_700_000_100, 0))
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if err := store.Complete(context.Background(), item.URL); err != nil {
		t.Fatal(err)
	}
	inserted, err = store.EnqueueBounded(context.Background(),
		[]FrontierItem{{URL: "https://a.example/5", Root: "example"}}, 3)
	if err != nil || inserted != 0 {
		t.Fatalf("EnqueueBounded() past spent budget = %d, %v, want 0 inserts", inserted, err)
	}
}

func TestEnqueueBoundedUnlimitedWhenBudgetUnset(t *testing.T) {
	store := openTestStore(t)
	inserted, err := store.EnqueueBounded(context.Background(), []FrontierItem{
		{URL: "https://a.example/1", Root: "example"},
		{URL: "https://a.example/2", Root: "example"},
		{URL: "https://a.example/3", Root: "example"},
	}, 0)
	if err != nil || inserted != 3 {
		t.Fatalf("EnqueueBounded(cap=0) = %d, %v, want 3", inserted, err)
	}
}

// TestActiveFrontierCountsPendingAndLeased locks the crawl-liveness signal:
// workers may only stop when no URL is waiting or in flight, because an
// in-flight page can still enqueue links that refill an empty frontier.
func TestActiveFrontierCountsPendingAndLeased(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.Enqueue(context.Background(), []FrontierItem{
		{URL: "https://a.example/1", Root: "example"},
		{URL: "https://b.example/1", Root: "b.example"},
	}); err != nil {
		t.Fatal(err)
	}
	if active, err := store.ActiveFrontier(context.Background()); err != nil || active != 2 {
		t.Fatalf("ActiveFrontier() = %d, %v, want 2", active, err)
	}
	item, ok, err := store.Lease(context.Background(), time.Unix(1_700_000_100, 0))
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if active, err := store.ActiveFrontier(context.Background()); err != nil || active != 2 {
		t.Fatalf("ActiveFrontier() with lease = %d, %v, want 2", active, err)
	}
	if err := store.Complete(context.Background(), item.URL); err != nil {
		t.Fatal(err)
	}
	if err := store.Fail(context.Background(), "https://b.example/1", 0); err != nil {
		t.Fatal(err)
	}
	// b.example went back to pending (attempts below the limit), a.example is done.
	if active, err := store.ActiveFrontier(context.Background()); err != nil || active != 1 {
		t.Fatalf("ActiveFrontier() after complete = %d, %v, want 1", active, err)
	}
}

// TestRequeueReopensFinishedRows locks the link-discovery bootstrap: on a
// frontier drained by earlier runs every seed row is done, no page is ever
// fetched again, and in-crawl discovery has no page to grow from. Requeue
// returns named URLs to pending so the seeds get refetched and remined.
func TestRequeueReopensFinishedRows(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.Enqueue(context.Background(), []FrontierItem{
		{URL: "https://a.example/", Root: "example"},
		{URL: "https://a.example/deep", Root: "example"},
		{URL: "https://b.example/", Root: "b.example"},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_100, 0)
	item, ok, err := store.Lease(context.Background(), now)
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if err := store.Complete(context.Background(), item.URL); err != nil {
		t.Fatal(err)
	}
	// Occupy a.example/deep so the next lease reaches b.example, then exhaust
	// b.example into the failed state.
	deep, ok, err := store.Lease(context.Background(), now)
	if err != nil || !ok || deep.URL != "https://a.example/deep" {
		t.Fatalf("Lease() = %#v, %v, %v", deep, ok, err)
	}
	leased, ok, err := store.Lease(context.Background(), now)
	if err != nil || !ok || leased.URL != "https://b.example/" {
		t.Fatalf("Lease() = %#v, %v, %v", leased, ok, err)
	}
	if err := store.Fail(context.Background(), "https://b.example/", 1); err != nil {
		t.Fatal(err)
	}
	// Reopen the done and failed rows; the unknown URL is a no-op and the
	// in-flight a.example/deep lease stays untouched.
	requeued, err := store.Requeue(context.Background(), []string{
		item.URL, "https://b.example/", "https://missing.example/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if requeued != 2 {
		t.Fatalf("Requeue() = %d, want the done and failed rows reopened", requeued)
	}
	if active, err := store.ActiveFrontier(context.Background()); err != nil || active != 3 {
		t.Fatalf("ActiveFrontier() after requeue = %d, %v, want all 3 rows open", active, err)
	}
}

func frontierHas(t *testing.T, store *Store, url string) bool {
	t.Helper()
	var one int
	err := store.db.QueryRow(`SELECT 1 FROM frontier WHERE url = ?`, url).Scan(&one)
	if err == nil {
		return true
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	t.Fatal(err)
	return false
}

func TestFrontierFailRetriesThenGivesUp(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.Enqueue(context.Background(), []FrontierItem{{URL: "https://a.example/", Root: "example"}}); err != nil {
		t.Fatal(err)
	}
	item, ok, err := store.Lease(context.Background(), time.Now())
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if err := store.Fail(context.Background(), item.URL, 2); err != nil {
		t.Fatal(err)
	}
	retried, ok, err := store.Lease(context.Background(), time.Now())
	if err != nil || !ok || retried.URL != item.URL {
		t.Fatalf("retry Lease() = %#v, %v, %v", retried, ok, err)
	}
	if err := store.Fail(context.Background(), retried.URL, 2); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Lease(context.Background(), time.Now()); err != nil || ok {
		t.Fatalf("failed URL was leased again, ok=%v err=%v", ok, err)
	}
}
