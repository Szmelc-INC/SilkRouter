package db

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrWriterClosed is returned by Writer methods after Close or after the
// writer's context was cancelled.
var ErrWriterClosed = errors.New("db: writer closed")

// Writer defaults.
const (
	DefaultBatchSize     = 500
	DefaultFlushInterval = time.Second
)

// WriterOptions tune a Writer.
type WriterOptions struct {
	// BatchSize commits once this many rows are pending (default 500).
	BatchSize int
	// FlushInterval commits pending rows at least this often (default 1s).
	FlushInterval time.Duration
	// OnError is called (from the writer goroutine) when a batch fails to
	// commit; the batch is dropped. The first error is also kept for Close.
	OnError func(err error, rows int)
}

// WriterStats counts what a Writer committed.
type WriterStats struct {
	Measurements int64 `json:"measurements"`
	Reputation   int64 `json:"reputation"`
	Infos        int64 `json:"infos"`
	Batches      int64 `json:"batches"`
	Failed       int64 `json:"failed_rows"`
}

// Writer is the single writer goroutine bulk producers (scans with hundreds
// of workers) send results to. It commits in batches — when BatchSize rows
// are pending or FlushInterval has passed — each batch in one transaction
// that inserts the facts and updates the proxies' summary columns.
//
// Every method is safe for concurrent use. A successful Measure/Reputation
// means the row was handed to the writer goroutine and will be committed
// (or reported through OnError): on Close, and on cancellation of the
// context given to NewWriter, pending rows are committed with a
// non-cancelled context before the goroutine exits, so Ctrl-C keeps what a
// scan already gathered.
type Writer struct {
	db    *DB
	opts  WriterOptions
	ctx   context.Context
	in    chan writeOp
	quit  chan struct{}
	done  chan struct{}
	once  sync.Once
	err   error // first commit error, read after done
	stats struct {
		measurements, reputation, infos, batches, failed atomic.Int64
	}
}

type writeOp struct {
	m     *Measurement
	r     *Reputation
	i     *ProxyInfo
	flush chan error
}

// NewWriter starts a writer goroutine. Cancelling ctx stops it (after
// committing what is pending); Close does the same explicitly.
func (d *DB) NewWriter(ctx context.Context, opts WriterOptions) *Writer {
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultBatchSize
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = DefaultFlushInterval
	}
	w := &Writer{
		db: d, opts: opts, ctx: ctx,
		in:   make(chan writeOp), // unbuffered: accepted == owned by the loop
		quit: make(chan struct{}),
		done: make(chan struct{}),
	}
	go w.loop()
	return w
}

// Measure queues a measurement (a zero TS is stamped now, not at commit
// time). It blocks while a batch is being committed (back-pressure) and
// returns ErrWriterClosed once the writer has stopped.
func (w *Writer) Measure(m Measurement) error {
	if m.TS.IsZero() {
		m.TS = nowFunc()
	}
	return w.send(writeOp{m: &m})
}

// Reputation queues a reputation verdict.
func (w *Writer) Reputation(r Reputation) error {
	if r.CheckedAt.IsZero() {
		r.CheckedAt = nowFunc()
	}
	return w.send(writeOp{r: &r})
}

// Info queues scan-learned proxy information (geo, anonymity, protocols,
// ...). It is applied after the measurements and verdicts of its batch.
func (w *Writer) Info(i ProxyInfo) error {
	return w.send(writeOp{i: &i})
}

func (w *Writer) send(op writeOp) error {
	select {
	case <-w.done:
		return ErrWriterClosed
	default:
	}
	select {
	case w.in <- op:
		return nil
	case <-w.done:
		return ErrWriterClosed
	}
}

// Flush commits everything queued so far and waits for it. It returns the
// commit error of this flush, if any. On a writer that already stopped
// (Close, or its context was cancelled — which committed everything it had
// accepted) it returns the first commit error instead of ErrWriterClosed.
func (w *Writer) Flush() error {
	ch := make(chan error, 1)
	if err := w.send(writeOp{flush: ch}); err != nil {
		<-w.done
		return w.err
	}
	select {
	case err := <-ch:
		return err
	case <-w.done:
		// The loop exited (cancellation) after accepting the flush; it
		// committed pending rows on the way out.
		select {
		case err := <-ch:
			return err
		default:
			return w.err
		}
	}
}

// Close commits pending rows, stops the goroutine and returns the first
// commit error seen during the writer's life. It is idempotent.
func (w *Writer) Close() error {
	w.once.Do(func() { close(w.quit) })
	<-w.done
	return w.err
}

// Done is closed when the writer goroutine has exited.
func (w *Writer) Done() <-chan struct{} { return w.done }

// Stats returns what has been committed so far.
func (w *Writer) Stats() WriterStats {
	return WriterStats{
		Measurements: w.stats.measurements.Load(),
		Reputation:   w.stats.reputation.Load(),
		Infos:        w.stats.infos.Load(),
		Batches:      w.stats.batches.Load(),
		Failed:       w.stats.failed.Load(),
	}
}

func (w *Writer) loop() {
	defer close(w.done)
	ticker := time.NewTicker(w.opts.FlushInterval)
	defer ticker.Stop()

	var ms []Measurement
	var rs []Reputation
	var is []ProxyInfo
	commit := func(ctx context.Context) error {
		if len(ms) == 0 && len(rs) == 0 && len(is) == 0 {
			return nil
		}
		err := w.commit(ctx, ms, rs, is)
		ms, rs, is = ms[:0], rs[:0], is[:0]
		return err
	}
	// The final commit must survive the cancellation that triggered it.
	final := context.WithoutCancel(w.ctx)

	for {
		select {
		case op := <-w.in:
			switch {
			case op.m != nil:
				ms = append(ms, *op.m)
			case op.r != nil:
				rs = append(rs, *op.r)
			case op.i != nil:
				is = append(is, *op.i)
			case op.flush != nil:
				op.flush <- commit(final)
				continue
			}
			if len(ms)+len(rs)+len(is) >= w.opts.BatchSize {
				commit(w.ctx)
			}
		case <-ticker.C:
			commit(w.ctx)
		case <-w.quit:
			commit(final)
			return
		case <-w.ctx.Done():
			commit(final)
			return
		}
	}
}

// commit writes one batch in a single transaction. A batch that fails
// because the context was cancelled mid-way is retried once with a
// non-cancelled context.
func (w *Writer) commit(ctx context.Context, ms []Measurement, rs []Reputation, is []ProxyInfo) error {
	run := func(ctx context.Context) error {
		// Work on copies: recordTx fills IDs/defaults in place, and a retry
		// must start from the original rows.
		mc := append([]Measurement(nil), ms...)
		rc := append([]Reputation(nil), rs...)
		ic := append([]ProxyInfo(nil), is...)
		return w.db.Tx(ctx, func(tx *sql.Tx) error {
			if len(mc) > 0 {
				if err := recordTx(ctx, tx, mc); err != nil {
					return err
				}
			}
			if len(rc) > 0 {
				if err := reputationTx(ctx, tx, rc); err != nil {
					return err
				}
			}
			if len(ic) > 0 {
				return infoTx(ctx, tx, ic)
			}
			return nil
		})
	}
	err := run(ctx)
	if err != nil && ctx.Err() != nil {
		err = run(context.WithoutCancel(ctx))
	}
	n := len(ms) + len(rs) + len(is)
	if err != nil {
		w.stats.failed.Add(int64(n))
		if w.err == nil {
			w.err = err
		}
		if w.opts.OnError != nil {
			w.opts.OnError(err, n)
		}
		return err
	}
	w.stats.measurements.Add(int64(len(ms)))
	w.stats.reputation.Add(int64(len(rs)))
	w.stats.infos.Add(int64(len(is)))
	w.stats.batches.Add(1)
	return nil
}
