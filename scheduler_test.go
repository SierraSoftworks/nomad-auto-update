package main

import (
	"container/heap"
	"context"
	"testing"
	"time"
)

var schedEpoch = time.Unix(1_700_000_000, 0)

func newTestScheduler(nomad nomadAPI) *scheduler {
	s := newScheduler(newUpdater(nomad, false, newVersionCache("")), nomad, 5*time.Minute, time.Hour, 2)
	s.now = func() time.Time { return schedEpoch }
	s.splay = func(time.Duration) time.Duration { return 0 }
	return s
}

func TestCheckHeapOrdersByDueTime(t *testing.T) {
	h := &checkHeap{}
	heap.Push(h, &checkItem{key: "c", due: schedEpoch.Add(3 * time.Minute)})
	heap.Push(h, &checkItem{key: "a", due: schedEpoch.Add(1 * time.Minute)})
	heap.Push(h, &checkItem{key: "b", due: schedEpoch.Add(2 * time.Minute)})

	var order []string
	for h.Len() > 0 {
		order = append(order, heap.Pop(h).(*checkItem).key)
	}
	if want := []string{"a", "b", "c"}; !equalStrings(order, want) {
		t.Fatalf("pop order = %v, want %v", order, want)
	}
}

func TestDefaultSplayWithinInterval(t *testing.T) {
	for i := 0; i < 1000; i++ {
		d := defaultSplay(time.Hour)
		if d < 0 || d >= time.Hour {
			t.Fatalf("splay %s out of [0, 1h)", d)
		}
	}
	if defaultSplay(0) != 0 {
		t.Fatal("splay of a zero interval must be zero")
	}
}

func TestSchedulerDiscoverAddsAndRemoves(t *testing.T) {
	nomad := &fakeNomad{jobs: []managedJob{
		{Namespace: "default", ID: "a"},
		{Namespace: "default", ID: "b"},
	}}
	s := newTestScheduler(nomad)

	s.discover(context.Background())
	if len(s.managed) != 2 || s.queue.Len() != 2 {
		t.Fatalf("after first discovery: managed=%d queue=%d", len(s.managed), s.queue.Len())
	}

	// b vanishes, c appears.
	nomad.jobs = []managedJob{
		{Namespace: "default", ID: "a"},
		{Namespace: "default", ID: "c"},
	}
	s.discover(context.Background())
	if len(s.managed) != 2 {
		t.Fatalf("managed = %d, want 2", len(s.managed))
	}
	if _, ok := s.managed["default/b"]; ok {
		t.Fatal("job b should have been removed")
	}
	if _, ok := s.managed["default/c"]; !ok {
		t.Fatal("job c should have been added")
	}
	if s.queue.Len() != 2 {
		t.Fatalf("queue = %d, want 2 (a retained, c added, b removed)", s.queue.Len())
	}
}

func TestSchedulerDiscoverKeepsScheduleOnError(t *testing.T) {
	nomad := &fakeNomad{jobs: []managedJob{{Namespace: "default", ID: "a"}}}
	s := newTestScheduler(nomad)
	s.discover(context.Background())

	nomad.listErr = context.DeadlineExceeded
	s.discover(context.Background())
	if len(s.managed) != 1 {
		t.Fatalf("managed set should be retained on discovery error, got %d", len(s.managed))
	}
}

func TestTimeUntilNext(t *testing.T) {
	s := newTestScheduler(&fakeNomad{})

	if got := s.timeUntilNext(); got != s.discoverEvery {
		t.Fatalf("empty queue wait = %s, want %s", got, s.discoverEvery)
	}

	heap.Push(s.queue, &checkItem{key: "future", due: schedEpoch.Add(2 * time.Minute)})
	if got := s.timeUntilNext(); got != 2*time.Minute {
		t.Fatalf("wait = %s, want 2m", got)
	}

	s.queue.remove("future")
	heap.Push(s.queue, &checkItem{key: "past", due: schedEpoch.Add(-time.Minute)})
	if got := s.timeUntilNext(); got != 0 {
		t.Fatalf("overdue wait = %s, want 0", got)
	}

	s.queue.remove("past")
	heap.Push(s.queue, &checkItem{key: "far", due: schedEpoch.Add(time.Hour)})
	if got := s.timeUntilNext(); got != s.discoverEvery {
		t.Fatalf("far-future wait = %s, want cap %s", got, s.discoverEvery)
	}
}

func TestRescheduleUsesJobInterval(t *testing.T) {
	s := newTestScheduler(&fakeNomad{})
	s.managed["default/a"] = managedJob{Namespace: "default", ID: "a", Interval: 30 * time.Minute}
	s.inflight["default/a"] = true

	s.reschedule("default/a")
	if s.inflight["default/a"] {
		t.Fatal("job should no longer be in flight")
	}
	if s.queue.Len() != 1 {
		t.Fatalf("queue = %d, want 1", s.queue.Len())
	}
	if due := s.queue.peek().due; !due.Equal(schedEpoch.Add(30 * time.Minute)) {
		t.Fatalf("next due = %s, want %s", due, schedEpoch.Add(30*time.Minute))
	}
}

func TestRescheduleDropsRemovedJob(t *testing.T) {
	s := newTestScheduler(&fakeNomad{})
	s.inflight["default/gone"] = true

	s.reschedule("default/gone")
	if s.queue.Len() != 0 {
		t.Fatal("a job removed while in flight must not be requeued")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
