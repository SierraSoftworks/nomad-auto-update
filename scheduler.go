package main

import (
	"container/heap"
	"context"
	"math/rand/v2"
	"sort"
	"time"

	humane "github.com/sierrasoftworks/humane-errors-go"
	"go.uber.org/zap"
)

// checkItem is a single scheduled check in the scheduler's queue.
type checkItem struct {
	key   string
	due   time.Time
	index int
}

// checkHeap is a min-heap of pending checks ordered by their due time.
type checkHeap []*checkItem

func (h checkHeap) Len() int           { return len(h) }
func (h checkHeap) Less(i, j int) bool { return h[i].due.Before(h[j].due) }
func (h checkHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].index = i; h[j].index = j }
func (h checkHeap) peek() *checkItem   { return h[0] }

func (h *checkHeap) Push(x any) {
	it := x.(*checkItem)
	it.index = len(*h)
	*h = append(*h, it)
}

func (h *checkHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	old[n-1] = nil
	it.index = -1
	*h = old[:n-1]
	return it
}

// remove deletes the queued check for key, if present.
func (h *checkHeap) remove(key string) {
	for i, it := range *h {
		if it.key == key {
			heap.Remove(h, i)
			return
		}
	}
}

// scheduler drives auto-update checks from a single central queue. A periodic
// discovery pass keeps the managed set in sync with Nomad; each managed job is
// checked on its own interval, with the first check splayed across the
// interval so checks don't stampede. All queue and map state is owned by the
// run loop's goroutine; workers touch only the semaphore and results channel.
type scheduler struct {
	updater *updater
	nomad   nomadAPI

	discoverEvery   time.Duration
	defaultInterval time.Duration
	checkTimeout    time.Duration
	concurrency     int

	now   func() time.Time
	after func(time.Duration) <-chan time.Time
	splay func(time.Duration) time.Duration

	managed  map[string]managedJob
	inflight map[string]bool
	queue    *checkHeap
	results  chan string
	sem      chan struct{}
}

func newScheduler(u *updater, nomad nomadAPI, discoverEvery, defaultInterval time.Duration, concurrency int) *scheduler {
	if concurrency < 1 {
		concurrency = 1
	}
	q := &checkHeap{}
	return &scheduler{
		updater:         u,
		nomad:           nomad,
		discoverEvery:   discoverEvery,
		defaultInterval: defaultInterval,
		checkTimeout:    2 * time.Minute,
		concurrency:     concurrency,
		now:             time.Now,
		after:           time.After,
		splay:           defaultSplay,
		managed:         map[string]managedJob{},
		inflight:        map[string]bool{},
		queue:           q,
		results:         make(chan string, concurrency),
		sem:             make(chan struct{}, concurrency),
	}
}

// defaultSplay returns a uniform random offset in [0, interval).
func defaultSplay(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(interval)))
}

// run drives the scheduler until ctx is cancelled.
func (s *scheduler) run(ctx context.Context) {
	s.discover(ctx)
	discoverCh := s.after(s.discoverEvery)

	for {
		timer := s.after(s.timeUntilNext())
		select {
		case <-ctx.Done():
			return
		case <-discoverCh:
			s.discover(ctx)
			discoverCh = s.after(s.discoverEvery)
		case key := <-s.results:
			s.reschedule(key)
		case <-timer:
			s.runDue(ctx)
		}
	}
}

// runOnce performs a single discovery pass and checks every managed job once,
// sequentially, then returns. It backs the -once flag.
func (s *scheduler) runOnce(ctx context.Context) {
	s.discover(ctx)

	keys := make([]string, 0, len(s.managed))
	for k := range s.managed {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		if ctx.Err() != nil {
			return
		}
		job := s.managed[k]
		cctx, cancel := context.WithTimeout(ctx, s.checkTimeout)
		if _, err := s.updater.check(cctx, job); err != nil {
			log(ctx).With(zap.String("job", job.key())).Error("check failed", humane.Zap(err)...)
		}
		cancel()
	}
}

// timeUntilNext reports how long to wait before the earliest queued check is
// due, capped at the discovery interval so the loop re-evaluates regularly.
func (s *scheduler) timeUntilNext() time.Duration {
	if s.queue.Len() == 0 {
		return s.discoverEvery
	}
	d := s.queue.peek().due.Sub(s.now())
	if d < 0 {
		d = 0
	}
	if d > s.discoverEvery {
		d = s.discoverEvery
	}
	return d
}

// runDue dispatches every check whose due time has passed to a worker.
func (s *scheduler) runDue(ctx context.Context) {
	now := s.now()
	for s.queue.Len() > 0 && !s.queue.peek().due.After(now) {
		item := heap.Pop(s.queue).(*checkItem)
		job, ok := s.managed[item.key]
		if !ok || s.inflight[item.key] {
			continue
		}
		s.inflight[item.key] = true
		go s.worker(ctx, job)
	}
}

// worker runs a single check under the concurrency semaphore and reports
// completion back to the run loop for rescheduling.
func (s *scheduler) worker(ctx context.Context, job managedJob) {
	s.sem <- struct{}{}
	defer func() { <-s.sem }()

	cctx, cancel := context.WithTimeout(ctx, s.checkTimeout)
	defer cancel()
	if _, err := s.updater.check(cctx, job); err != nil {
		log(ctx).With(zap.String("job", job.key())).Error("check failed", humane.Zap(err)...)
	}

	select {
	case s.results <- job.key():
	case <-ctx.Done():
	}
}

// reschedule requeues a completed job's next check, unless it was removed while
// running.
func (s *scheduler) reschedule(key string) {
	delete(s.inflight, key)
	job, ok := s.managed[key]
	if !ok {
		return
	}
	heap.Push(s.queue, &checkItem{key: key, due: s.now().Add(s.intervalFor(job))})
}

// discover refreshes the managed set from Nomad, enqueuing newly seen jobs with
// a splayed first check and dropping jobs that have gone away.
func (s *scheduler) discover(ctx context.Context) {
	jobs, err := s.nomad.listAutoUpdateJobs(ctx)
	if err != nil {
		log(ctx).Warn("discovery failed, keeping current schedule", humane.Zap(err)...)
		return
	}

	seen := make(map[string]bool, len(jobs))
	for _, job := range jobs {
		key := job.key()
		seen[key] = true
		_, existed := s.managed[key]
		s.managed[key] = job
		if !existed && !s.inflight[key] {
			heap.Push(s.queue, &checkItem{key: key, due: s.now().Add(s.splay(s.intervalFor(job)))})
		}
	}

	for key := range s.managed {
		if !seen[key] {
			delete(s.managed, key)
			s.queue.remove(key)
		}
	}

	mJobsManaged.Record(ctx, int64(len(s.managed)))
	log(ctx).Debug("discovery complete", zap.Int("managed_jobs", len(s.managed)))
}

// intervalFor returns a job's check interval, defaulting when unset.
func (s *scheduler) intervalFor(job managedJob) time.Duration {
	if job.Interval > 0 {
		return job.Interval
	}
	return s.defaultInterval
}
