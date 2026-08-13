package searchindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	// Completing the two-URL root's lease frees that root for its next URL.
	twoURLRoot := first
	if twoURLRoot.Root != "example" {
		twoURLRoot = second
	}
	if err := store.Complete(context.Background(), twoURLRoot.URL); err != nil {
		t.Fatal(err)
	}
	again, ok, err := store.Lease(context.Background(), now.Add(time.Second))
	if err != nil || !ok || again.Root != "example" {
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

// TestFrontierLeaseDoesNotStarveLaterSortingRoots locks lease fairness: with
// one global URL ordering the crawl wedges into whatever sorts first (every
// http:// URL precedes every https:// one), and a root landing late in the
// order never gets a worker while link discovery keeps refilling the front.
func TestFrontierLeaseDoesNotStarveLaterSortingRoots(t *testing.T) {
	store := openTestStore(t)
	items := make([]FrontierItem, 0, 301)
	for index := range 300 {
		items = append(items, FrontierItem{
			URL:  fmt.Sprintf("http://early.example/%03d", index),
			Root: "early.example",
		})
	}
	items = append(items, FrontierItem{URL: "https://late.example/", Root: "late.example"})
	if _, err := store.Enqueue(context.Background(), items); err != nil {
		t.Fatal(err)
	}
	// Forty fair picks over two roots miss one with probability 2^-40; the
	// old global order needed all 300 early rows drained first.
	for range 40 {
		item, ok, err := store.Lease(context.Background(), time.Now())
		if err != nil || !ok {
			t.Fatal(err, ok)
		}
		if item.Root == "late.example" {
			return
		}
		if err := store.Complete(context.Background(), item.URL); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("late-sorting root was never leased in 40 picks")
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
	otherURL := "https://b.example/1"
	if item.URL == otherURL {
		otherURL = "https://a.example/1"
	}
	if err := store.Fail(context.Background(), otherURL, 0); err != nil {
		t.Fatal(err)
	}
	// The failed row went back to pending (attempts below the limit), the
	// completed one is done.
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
	// Two leases claim both roots in whatever order the fair pick lands on;
	// within a root the first URL is deterministic.
	now := time.Unix(1_700_000_100, 0)
	byURL := map[string]bool{}
	for range 2 {
		item, ok, err := store.Lease(context.Background(), now)
		if err != nil || !ok {
			t.Fatal(err, ok)
		}
		byURL[item.URL] = true
	}
	if !byURL["https://a.example/"] || !byURL["https://b.example/"] {
		t.Fatalf("leases = %#v, want both roots' first URLs", byURL)
	}
	if err := store.Complete(context.Background(), "https://a.example/"); err != nil {
		t.Fatal(err)
	}
	// b.example was leased once, so one more failure exhausts it.
	if err := store.Fail(context.Background(), "https://b.example/", 1); err != nil {
		t.Fatal(err)
	}
	// Reopen the done and failed rows; the unknown URL is a no-op and the
	// still-pending a.example/deep row stays untouched.
	requeued, err := store.Requeue(context.Background(), []string{
		"https://a.example/", "https://b.example/", "https://missing.example/",
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

// TestRequeueStarvedRootsReopensSmallRootsOnly locks the legacy-index
// recovery: pages fetched before in-crawl link discovery existed were never
// mined for links, and once such a root's rows are all closed it can never
// grow. Reopening is bounded to small roots so a sitemap-fed site with
// thousands of already-mined rows is not refetched wholesale.
func TestRequeueStarvedRootsReopensSmallRootsOnly(t *testing.T) {
	store := openTestStore(t)
	items := []FrontierItem{
		{URL: "https://small.example/", Root: "small.example"},
		{URL: "https://small.example/a", Root: "small.example"},
	}
	for index := range 5 {
		items = append(items, FrontierItem{
			URL:  fmt.Sprintf("https://big.example/%d", index),
			Root: "big.example",
		})
	}
	if _, err := store.Enqueue(context.Background(), items); err != nil {
		t.Fatal(err)
	}
	closeAllFrontierRows(t, store)

	reopened, err := store.RequeueStarvedRoots(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if reopened != 2 {
		t.Fatalf("RequeueStarvedRoots() = %d, want small.example's 2 rows", reopened)
	}
	active, err := store.ActiveFrontier(context.Background())
	if err != nil || active != 2 {
		t.Fatalf("ActiveFrontier() = %d, %v, want only the starved root open", active, err)
	}
}

func closeAllFrontierRows(t *testing.T, store *Store) {
	t.Helper()
	for {
		item, ok, err := store.Lease(context.Background(), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			active, err := store.ActiveFrontier(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if active == 0 {
				return
			}
			continue
		}
		if err := store.Complete(context.Background(), item.URL); err != nil {
			t.Fatal(err)
		}
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
