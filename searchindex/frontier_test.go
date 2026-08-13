package searchindex

import (
	"context"
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
