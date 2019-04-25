package taskq

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	redlock "github.com/bsm/redis-lock"
	"golang.org/x/time/rate"

	"github.com/vmihailenco/taskq/internal"
)

const timePrecision = time.Microsecond
const stopTimeout = 30 * time.Second
const workerIdleTimeout = 3 * time.Second
const autotuneResetPeriod = 5 * time.Minute

var ErrAsyncTask = errors.New("taskq: async task")

type Delayer interface {
	Delay() time.Duration
}

type ConsumerStats struct {
	WorkerNumber  uint32
	FetcherNumber uint32
	BufferSize    uint32
	Buffered      uint32
	InFlight      uint32
	Processed     uint32
	Retries       uint32
	Fails         uint32
	AvgDuration   time.Duration
	MinDuration   time.Duration
	MaxDuration   time.Duration
}

type limiter struct {
	bucket  string
	limiter RateLimiter
	limit   rate.Limit

	allowedCount uint32 // atomic
	cancelled    uint32 // atomic
}

func (l *limiter) Reserve(max int) int {
	if l.limiter == nil {
		return max
	}

	for {
		cancelled := atomic.LoadUint32(&l.cancelled)
		if cancelled == 0 {
			break
		}

		if cancelled >= uint32(max) {
			if atomic.CompareAndSwapUint32(&l.cancelled, cancelled, uint32(max)-1) {
				return max
			}
			continue
		}

		if atomic.CompareAndSwapUint32(&l.cancelled, cancelled, uint32(cancelled)-1) {
			return int(cancelled)
		}
	}

	var size int
	for {
		delay, allow := l.limiter.AllowRate(l.bucket, l.limit)
		if allow {
			size++
			if size == max {
				atomic.AddUint32(&l.allowedCount, 1)
				return size
			}
			continue
		} else {
			atomic.StoreUint32(&l.allowedCount, 0)
		}

		if size > 0 {
			return size
		}
		time.Sleep(delay)
	}
}

func (l *limiter) Cancel(n int) {
	if l.limiter == nil {
		return
	}
	atomic.AddUint32(&l.cancelled, uint32(n))
}

func (l *limiter) Limited() bool {
	return l.limiter != nil && atomic.LoadUint32(&l.allowedCount) < 3
}

// Consumer reserves messages from the queue, processes them,
// and then either releases or deletes messages from the queue.
type Consumer struct {
	q   Queue
	opt *QueueOptions

	rand    *rand.Rand
	buffer  chan *Message
	limiter *limiter

	stopCh chan struct{}

	workerNumber  int32 // atomic
	fetcherNumber int32 // atomic

	jobsWG sync.WaitGroup

	errCount uint32
	delaySec uint32

	starving int
	loaded   int

	fetcherIdle uint32 // atomic
	fetcherBusy uint32 // atomic
	workerIdle  uint32 // atomic
	workerBusy  uint32 // atomic

	lastAutotuneReset time.Time

	inFlight    uint32
	deleting    uint32
	processed   uint32
	fails       uint32
	retries     uint32
	avgDuration uint32
	minDuration uint32
	maxDuration uint32
}

// New creates new Consumer for the queue using provided processing options.
func NewConsumer(q Queue) *Consumer {
	opt := q.Options()
	p := &Consumer{
		q:   q,
		opt: opt,

		rand:   rand.New(rand.NewSource(time.Now().UnixNano())),
		buffer: make(chan *Message, opt.BufferSize),
		limiter: &limiter{
			bucket:  q.Name(),
			limiter: opt.RateLimiter,
			limit:   opt.RateLimit,
		},
	}

	return p
}

// Starts creates new Consumer and starts it.
func StartConsumer(q Queue) *Consumer {
	c := NewConsumer(q)
	if err := c.Start(); err != nil {
		panic(err)
	}
	return c
}

func (c *Consumer) Queue() Queue {
	return c.q
}

func (c *Consumer) Options() *QueueOptions {
	return c.opt
}

// Stats returns processor stats.
func (p *Consumer) Stats() *ConsumerStats {
	return &ConsumerStats{
		WorkerNumber:  uint32(atomic.LoadInt32(&p.workerNumber)),
		FetcherNumber: uint32(atomic.LoadInt32(&p.fetcherNumber)),
		BufferSize:    uint32(cap(p.buffer)),
		Buffered:      uint32(len(p.buffer)),
		InFlight:      atomic.LoadUint32(&p.inFlight),
		Processed:     atomic.LoadUint32(&p.processed),
		Retries:       atomic.LoadUint32(&p.retries),
		Fails:         atomic.LoadUint32(&p.fails),
		AvgDuration:   time.Duration(atomic.LoadUint32(&p.avgDuration)) * timePrecision,
		MinDuration:   time.Duration(atomic.LoadUint32(&p.minDuration)) * timePrecision,
		MaxDuration:   time.Duration(atomic.LoadUint32(&p.maxDuration)) * timePrecision,
	}
}

func (c *Consumer) Add(msg *Message) error {
	if msg.Delay > 0 {
		time.AfterFunc(msg.Delay, func() {
			msg.Delay = 0
			c.add(msg)
		})
	} else {
		c.add(msg)
	}
	return nil
}

func (c *Consumer) Len() int {
	return len(c.buffer)
}

func (c *Consumer) add(msg *Message) {
	_ = c.limiter.Reserve(1)
	c.buffer <- msg
}

// Start starts consuming messages in the queue.
func (p *Consumer) Start() error {
	if p.stopCh != nil {
		return errors.New("taskq: Consumer is already started")
	}

	stop := make(chan struct{})
	p.stopCh = stop

	atomic.StoreInt32(&p.fetcherNumber, 0)
	atomic.StoreInt32(&p.workerNumber, 0)

	for i := 0; i < p.opt.MinWorkers; i++ {
		p.addWorker(stop)
	}

	p.jobsWG.Add(1)
	go func() {
		defer p.jobsWG.Done()
		p.autotune(stop)
	}()

	return nil
}

func (c *Consumer) addWorker(stop <-chan struct{}) int32 {
	for {
		id := atomic.LoadInt32(&c.workerNumber)
		if id >= int32(c.opt.MaxWorkers) {
			return -1
		}
		if atomic.CompareAndSwapInt32(&c.workerNumber, id, id+1) {
			c.jobsWG.Add(1)
			go func() {
				defer c.jobsWG.Done()
				c.worker(id, stop)
			}()
			return id
		}
	}
}

func (c *Consumer) removeWorker() int32 {
	for {
		id := atomic.LoadInt32(&c.workerNumber)
		if id == 0 {
			return -1
		}
		if atomic.CompareAndSwapInt32(&c.workerNumber, id, id-1) {
			return id
		}
	}
}

func (c *Consumer) addFetcher(stop <-chan struct{}) int32 {
	for {
		id := atomic.LoadInt32(&c.fetcherNumber)
		if id >= int32(c.opt.MaxFetchers) {
			return -1
		}
		if c.tryStartFetcher(id, stop) {
			return id
		}
	}
}

func (c *Consumer) tryStartFetcher(id int32, stop <-chan struct{}) bool {
	if atomic.CompareAndSwapInt32(&c.fetcherNumber, id, id+1) {
		c.jobsWG.Add(1)
		go func() {
			defer c.jobsWG.Done()
			c.fetcher(id, stop)
		}()
		return true
	}
	return false
}

func (c *Consumer) removeFetcher() int32 {
	for {
		id := atomic.LoadInt32(&c.fetcherNumber)
		if id == 0 {
			return -1
		}
		if atomic.CompareAndSwapInt32(&c.fetcherNumber, id, id-1) {
			return id
		}
	}
}

func (c *Consumer) autotune(stop <-chan struct{}) {
	timer := time.NewTimer(time.Minute)
	timer.Stop()

	for {
		timeout := time.Duration(2000+c.rand.Intn(2000)) * time.Millisecond
		timer.Reset(timeout)

		select {
		case <-stop:
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
			c._autotune(stop)
		}
	}
}

func (c *Consumer) _autotune(stop <-chan struct{}) {
	if time.Since(c.lastAutotuneReset) > autotuneResetPeriod {
		c.resetAutotune()
		c.lastAutotuneReset = time.Now()
	}

	c.updateBuffered()

	if c.isStarving() {
		internal.Logger.Printf("%s: adding a fetcher", c)
		c.addFetcher(stop)
		c.resetAutotune()
		return
	}

	if c.hasIdleFetcher() {
		internal.Logger.Printf("%s: removing idle fetcher", c)
		c.removeFetcher()
		c.resetAutotune()
	}

	if c.isLoaded() {
		internal.Logger.Printf("%s: adding a worker", c)
		c.addWorker(stop)
		c.resetAutotune()
		return
	}

	if c.hasIdleWorker() {
		internal.Logger.Printf("%s: removing idle worker", c)
		c.removeWorker()
		c.resetAutotune()
	}
}

func (p *Consumer) hasFetcher() bool {
	return atomic.LoadInt32(&p.fetcherNumber) > 0
}

// Stop is StopTimeout with 30 seconds timeout.
func (p *Consumer) Stop() error {
	return p.StopTimeout(stopTimeout)
}

// StopTimeout waits workers for timeout duration to finish processing current
// messages and stops workers.
func (p *Consumer) StopTimeout(timeout time.Duration) error {
	if p.stopCh == nil || closed(p.stopCh) {
		return nil
	}
	close(p.stopCh)
	p.stopCh = nil

	done := make(chan struct{}, 1)
	go func() {
		p.jobsWG.Wait()
		done <- struct{}{}
	}()

	timer := time.NewTimer(timeout)
	var err error
	select {
	case <-done:
		timer.Stop()
	case <-timer.C:
		err = fmt.Errorf("workers are not stopped after %s", timeout)
	}

	return err
}

func (p *Consumer) paused() time.Duration {
	const threshold = 100

	if p.opt.PauseErrorsThreshold == 0 ||
		atomic.LoadUint32(&p.errCount) < uint32(p.opt.PauseErrorsThreshold) {
		return 0
	}

	sec := atomic.LoadUint32(&p.delaySec)
	if sec == 0 {
		return time.Minute
	}
	return time.Duration(sec) * time.Second
}

// ProcessAll starts workers to process messages in the queue and then stops
// them when all messages are processed.
func (p *Consumer) ProcessAll() error {
	if err := p.Start(); err != nil {
		return err
	}

	var prev *ConsumerStats
	var noWork int
	for {
		st := p.Stats()
		if prev != nil &&
			st.Buffered == 0 &&
			st.InFlight == 0 &&
			st.Processed == prev.Processed {
			noWork++
			if noWork == 2 {
				break
			}
		} else {
			noWork = 0
		}
		prev = st
		time.Sleep(time.Second)
	}

	return p.Stop()
}

// ProcessOne processes at most one message in the queue.
func (p *Consumer) ProcessOne() error {
	msg, err := p.reserveOne()
	if err != nil {
		return err
	}

	// TODO: wait
	return p.process(msg)
}

func (p *Consumer) reserveOne() (*Message, error) {
	select {
	case msg := <-p.buffer:
		return msg, nil
	default:
	}

	msgs, err := p.q.ReserveN(1, p.opt.WaitTimeout)
	if err != nil && err != internal.ErrNotSupported {
		return nil, err
	}

	if len(msgs) == 0 {
		return nil, errors.New("taskq: queue is empty")
	}
	if len(msgs) != 1 {
		return nil, fmt.Errorf("taskq: queue returned %d messages", len(msgs))
	}

	return &msgs[0], nil
}

func (p *Consumer) fetcher(id int32, stop <-chan struct{}) {
	timer := time.NewTimer(time.Minute)
	timer.Stop()

	fetchTimeout := p.opt.ReservationTimeout
	fetchTimeout -= fetchTimeout / 10

	for {
		if closed(stop) {
			break
		}

		if id >= atomic.LoadInt32(&p.fetcherNumber) {
			break
		}

		if pauseTime := p.paused(); pauseTime > 0 {
			p.resetPause()
			internal.Logger.Printf("%s is automatically paused for dur=%s", p.q, pauseTime)
			time.Sleep(pauseTime)
			continue
		}

		timer.Reset(fetchTimeout)
		timeout, err := p.fetchMessages(id, timer.C)
		if err != nil {
			if err == internal.ErrNotSupported {
				break
			}

			const backoff = time.Second
			internal.Logger.Printf(
				"%s fetchMessages failed: %s (sleeping for dur=%s)",
				p.q, err, backoff,
			)
			time.Sleep(backoff)
		}
		if timeout {
			break
		}

		if !timer.Stop() {
			<-timer.C
		}
	}
}

func (p *Consumer) fetchMessages(
	id int32, timeoutC <-chan time.Time,
) (timeout bool, err error) {
	size := p.limiter.Reserve(p.opt.ReservationSize)
	msgs, err := p.q.ReserveN(size, p.opt.WaitTimeout)
	if err != nil {
		return false, err
	}

	if d := size - len(msgs); d > 0 {
		p.limiter.Cancel(d)
	}

	if id > 0 {
		if len(msgs) < size {
			atomic.AddUint32(&p.fetcherIdle, 1)
		} else {
			atomic.AddUint32(&p.fetcherBusy, 1)
		}
	}

	for i := range msgs {
		msg := &msgs[i]

		select {
		case p.buffer <- msg:
		case <-timeoutC:
			for i := range msgs[i:] {
				p.release(&msgs[i], nil)
			}
			return true, nil
		}
	}

	return false, nil
}

func (p *Consumer) releaseBuffer() {
	for {
		msg := p.dequeueMessage()
		if msg == nil {
			break
		}
		p.release(msg, nil)
	}
}

func (p *Consumer) worker(workerID int32, stop <-chan struct{}) {
	var timer *time.Timer
	var timeout <-chan time.Time
	if workerID > 0 {
		timer = time.NewTimer(time.Minute)
		timer.Stop()
		timeout = timer.C
	}

	var lock *redlock.Locker
	if p.opt.WorkerLimit > 0 {
		key := fmt.Sprintf("%s:worker:lock:%d", p.q.Name(), workerID)
		lock = redlock.New(p.opt.Redis, key, &redlock.Options{
			LockTimeout: p.opt.ReservationTimeout + 10*time.Second,
		})
		defer p.unlockWorker(lock)
	}

	for {
		if workerID >= atomic.LoadInt32(&p.workerNumber) {
			return
		}

		if lock != nil {
			if !p.lockWorkerOrExit(lock, stop) {
				return
			}
		}

		if timer != nil {
			timer.Reset(workerIdleTimeout)
		}

		msg, timeout := p.waitMessage(stop, timeout)
		if timeout {
			atomic.AddUint32(&p.workerIdle, 1)
			continue
		}
		atomic.AddUint32(&p.workerBusy, 1)

		if timer != nil {
			if !timer.Stop() {
				<-timer.C
			}
		}

		if msg == nil {
			return
		}

		select {
		case <-stop:
			p.release(msg, nil)
		default:
			_ = p.process(msg)
		}
	}
}

func (c *Consumer) waitMessage(
	stop <-chan struct{}, timeoutC <-chan time.Time,
) (msg *Message, timeout bool) {
	msg = c.dequeueMessage()
	if msg != nil {
		return msg, false
	}

	c.tryStartFetcher(0, stop)

	select {
	case msg := <-c.buffer:
		return msg, false
	case <-stop:
		return c.dequeueMessage(), false
	case <-timeoutC:
		return nil, true
	}
}

func (c *Consumer) dequeueMessage() *Message {
	select {
	case msg := <-c.buffer:
		return msg
	default:
		return nil
	}
}

// Process is low-level API to process message bypassing the internal queue.
func (c *Consumer) Process(msg *Message) error {
	return c.process(msg)
}

func (c *Consumer) process(msg *Message) error {
	atomic.AddUint32(&c.inFlight, 1)

	if msg.Delay > 0 {
		err := c.q.Add(msg)
		if err != nil {
			return err
		}
		c.delete(msg, nil)
		return nil
	}

	if msg.StickyErr != nil {
		c.Put(msg, msg.StickyErr)
		return msg.StickyErr
	}

	err := c.q.HandleMessage(msg)
	if err == nil {
		c.resetPause()
	}
	if err != ErrAsyncTask {
		c.Put(msg, err)
	}
	return err
}

func (p *Consumer) Put(msg *Message, msgErr error) {
	if msgErr == nil {
		atomic.AddUint32(&p.processed, 1)
		p.delete(msg, msgErr)
		return
	}

	if msg.Task == nil {
		msg.Task = p.q.GetTask(msg.TaskName)
	}

	var opt *TaskOptions
	if msg.Task != nil {
		opt = msg.Task.Options()
	} else {
		opt = unknownTaskOpt
	}

	atomic.AddUint32(&p.errCount, 1)
	if msg.ReservedCount < opt.RetryLimit {
		msg.Delay = exponentialBackoff(
			opt.MinBackoff, opt.MaxBackoff, msg.ReservedCount)
		if msgErr != nil {
			if delayer, ok := msgErr.(Delayer); ok {
				msg.Delay = delayer.Delay()
			}
		}

		atomic.AddUint32(&p.retries, 1)
		p.release(msg, msgErr)
	} else {
		atomic.AddUint32(&p.fails, 1)
		p.delete(msg, msgErr)
	}
}

func (p *Consumer) release(msg *Message, msgErr error) {
	if msgErr != nil {
		new := uint32(msg.Delay / time.Second)
		for new > 0 {
			old := atomic.LoadUint32(&p.delaySec)
			if new > old {
				break
			}
			if atomic.CompareAndSwapUint32(&p.delaySec, old, new) {
				break
			}
		}

		internal.Logger.Printf("%s handler failed (will retry=%d in dur=%s): %s",
			msg.Task, msg.ReservedCount, msg.Delay, msgErr)
	}

	if err := p.q.Release(msg); err != nil {
		internal.Logger.Printf("%s Release failed: %s", msg.Task, err)
	}
	atomic.AddUint32(&p.inFlight, ^uint32(0))
}

func (p *Consumer) delete(msg *Message, err error) {
	if err != nil {
		internal.Logger.Printf("%s handler failed after retry=%d: %s",
			msg.Task, msg.ReservedCount, err)

		msg.StickyErr = err
		if err := p.q.HandleMessage(msg); err != nil {
			internal.Logger.Printf("%s fallback handler failed: %s", msg.Task, err)
		}
	}

	if err := p.q.Delete(msg); err != nil {
		internal.Logger.Printf("%s Delete failed: %s", msg.Task, err)
	}
	atomic.AddUint32(&p.inFlight, ^uint32(0))
}

// Purge discards messages from the internal queue.
func (p *Consumer) Purge() error {
	for {
		select {
		case msg := <-p.buffer:
			p.delete(msg, nil)
		default:
			return nil
		}
	}
}

func (p *Consumer) updateAvgDuration(dur time.Duration) {
	const decay = float32(1) / 30

	us := uint32(dur / timePrecision)
	if us == 0 {
		return
	}

	for {
		min := atomic.LoadUint32(&p.minDuration)
		if (min != 0 && us >= min) ||
			atomic.CompareAndSwapUint32(&p.minDuration, min, us) {
			break
		}
	}

	for {
		max := atomic.LoadUint32(&p.maxDuration)
		if us <= max || atomic.CompareAndSwapUint32(&p.maxDuration, max, us) {
			break
		}
	}

	for {
		avg := atomic.LoadUint32(&p.avgDuration)
		var newAvg uint32
		if avg > 0 {
			newAvg = uint32((1-decay)*float32(avg) + decay*float32(us))
		} else {
			newAvg = us
		}
		if atomic.CompareAndSwapUint32(&p.avgDuration, avg, newAvg) {
			break
		}
	}
}

func (p *Consumer) resetPause() {
	atomic.StoreUint32(&p.delaySec, 0)
	atomic.StoreUint32(&p.errCount, 0)
}

func (p *Consumer) lockWorkerOrExit(lock *redlock.Locker, stop <-chan struct{}) bool {
	timer := time.NewTimer(time.Minute)
	timer.Stop()

	for {
		ok, err := lock.Lock()
		if err != nil {
			internal.Logger.Printf("redlock.Lock failed: %s", err)
		}
		if ok {
			return true
		}

		timeout := time.Duration(500+p.rand.Intn(500)) * time.Millisecond
		timer.Reset(timeout)

		select {
		case <-stop:
			if !timer.Stop() {
				<-timer.C
			}
			return false
		case <-timer.C:
		}
	}
}

func (p *Consumer) unlockWorker(lock *redlock.Locker) {
	_ = lock.Unlock()
}

func (c *Consumer) String() string {
	fnum := atomic.LoadInt32(&c.fetcherNumber)
	wnum := atomic.LoadInt32(&c.workerNumber)
	inFlight := atomic.LoadUint32(&c.inFlight)

	var extra string
	if c.isStarving() {
		extra += " starving"
	}
	if c.isLoaded() {
		extra += " loaded"
	}
	if c.hasIdleFetcher() {
		extra += " idle-fetcher"
	}
	if c.hasIdleWorker() {
		extra += " idle-worker"
	}

	return fmt.Sprintf(
		"Consumer<%s %d/%d %d/%d%s>",
		c.q.Name(), fnum, len(c.buffer), inFlight, wnum, extra)
}

func (c *Consumer) updateBuffered() {
	buffered := len(c.buffer)
	if buffered == 0 {
		c.starving++
		c.loaded = 0
	} else if buffered > cap(c.buffer)/5*4 {
		c.starving = 0
		c.loaded++
	}
}

func (c *Consumer) isStarving() bool {
	if c.starving < 5 {
		return false
	}
	idle := atomic.LoadUint32(&c.fetcherIdle)
	busy := atomic.LoadUint32(&c.fetcherBusy)
	return busy > 10 && idle < busy
}

func (c *Consumer) isLoaded() bool {
	return c.loaded >= 5
}

func (c *Consumer) hasIdleFetcher() bool {
	num := atomic.LoadInt32(&c.fetcherNumber)
	if num <= 1 {
		return false
	}
	idle := atomic.LoadUint32(&c.fetcherIdle)
	busy := atomic.LoadUint32(&c.fetcherBusy)
	return busy > 10 && float64(idle) > float64(busy)/float64(num)
}

func (c *Consumer) hasIdleWorker() bool {
	num := atomic.LoadInt32(&c.workerNumber)
	if num <= 1 {
		return false
	}
	idle := atomic.LoadUint32(&c.workerIdle)
	busy := atomic.LoadUint32(&c.workerBusy)
	return busy > 10 && float64(idle) > float64(busy)/float64(num)
}

func (c *Consumer) resetAutotune() {
	c.starving = 0
	c.loaded = 0
	atomic.StoreUint32(&c.fetcherIdle, 0)
	atomic.StoreUint32(&c.fetcherBusy, 0)
	atomic.StoreUint32(&c.workerIdle, 0)
	atomic.StoreUint32(&c.workerBusy, 0)
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func exponentialBackoff(min, max time.Duration, retry int) time.Duration {
	var d time.Duration
	if retry > 0 {
		d = min << uint(retry-1)
	}
	if d < min {
		return min
	}
	if d > max {
		return max
	}
	return d
}
