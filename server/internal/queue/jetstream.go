package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"juju/internal/event"
)

// errCodeStoreFailed is the server's code for "the stream couldn't store
// it" (JSStreamStoreFailedErr), which with DiscardNew means the stream is at
// MaxBytes. nats.go has no constant for it.
const errCodeStoreFailed jetstream.ErrorCode = 10077

const deadMaxBytes = 256 << 20

const (
	eventsStream = "JUJU_EVENTS"
	deadStream   = "JUJU_DEAD"
	consumerName = "workers"
)

// JetStreamConfig says where the queue lives.
type JetStreamConfig struct {
	// URL of a NATS server with JetStream, shared by separate ingest and
	// worker processes. Empty: run an embedded NATS server inside juju,
	// storing the queue under Dir. No extra service to install, and no
	// network port: only this process can reach it.
	URL string
	Dir string
	// MaxBytes caps the queue's size on disk. Past it, ingest answers 429.
	MaxBytes int64
	// Fetch is how many deliveries a worker process asks for at a time.
	Fetch int
	// AckWait: an event a worker took but didn't acknowledge in this long
	// (it crashed mid-event) is delivered again. Default 30s.
	AckWait time.Duration
	Log     *slog.Logger
}

// JetStream is the durable queue: a NATS JetStream stream used as a work
// queue. Each event is a message whose ID is the event_id, so JetStream
// itself drops a duplicate published within two minutes (an SDK retrying
// after a lost response).
type JetStream struct {
	cfg JetStreamConfig
	ns  *server.Server // nil with an external URL
	nc  *nats.Conn
	js  jetstream.JetStream

	mu         sync.Mutex
	deliveries chan Delivery
	consume    jetstream.ConsumeContext
}

func OpenJetStream(ctx context.Context, cfg JetStreamConfig) (*JetStream, error) {
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = 1 << 30
	}
	if cfg.Fetch == 0 {
		cfg.Fetch = 64
	}
	if cfg.AckWait == 0 {
		cfg.AckWait = 30 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	q := &JetStream{cfg: cfg}
	var err error
	if cfg.URL == "" {
		opts := &server.Options{
			ServerName: "juju-embedded",
			JetStream:  true,
			StoreDir:   cfg.Dir,
			DontListen: true, // in-process connections only
			NoSigs:     true, // juju handles signals itself
			NoLog:      true,
			// JetStream reserves each stream's maximum up front, so this
			// must cover both streams; the events stream's MaxBytes is the
			// limit that matters.
			JetStreamMaxStore:  cfg.MaxBytes + deadMaxBytes + 64<<20,
			JetStreamMaxMemory: 64 << 20,
		}
		if q.ns, err = server.NewServer(opts); err != nil {
			return nil, fmt.Errorf("embedded nats: %w", err)
		}
		go q.ns.Start()
		if !q.ns.ReadyForConnections(30 * time.Second) {
			q.ns.Shutdown()
			return nil, errors.New("embedded nats did not start")
		}
		q.nc, err = nats.Connect("", nats.InProcessServer(q.ns))
	} else {
		q.nc, err = nats.Connect(cfg.URL, nats.Name("juju"), nats.MaxReconnects(-1))
	}
	if err != nil {
		q.Close()
		return nil, fmt.Errorf("connect to nats: %w", err)
	}
	if q.js, err = jetstream.New(q.nc); err != nil {
		q.Close()
		return nil, err
	}
	if err := q.setup(ctx); err != nil {
		q.Close()
		return nil, err
	}
	return q, nil
}

func (q *JetStream) setup(ctx context.Context) error {
	_, err := q.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     eventsStream,
		Subjects: []string{"juju.events.*"},
		// WorkQueue: a message is deleted once a worker acknowledges it, so
		// the stream only ever holds what's still to do.
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.FileStorage,
		MaxBytes:  q.cfg.MaxBytes,
		// When full, refuse new events (ingest answers 429 and the SDK
		// retries) rather than deleting old ones nobody has stored yet.
		Discard:    jetstream.DiscardNew,
		Duplicates: 2 * time.Minute,
	})
	if err != nil {
		return fmt.Errorf("create stream %s: %w", eventsStream, err)
	}
	// Events given up on, kept for a week for a person to look at.
	_, err = q.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name: deadStream, Subjects: []string{"juju.dead.*"}, Storage: jetstream.FileStorage,
		MaxAge: 7 * 24 * time.Hour, MaxBytes: deadMaxBytes, Discard: jetstream.DiscardOld,
	})
	if err != nil {
		return fmt.Errorf("create stream %s: %w", deadStream, err)
	}
	_, err = q.js.CreateOrUpdateConsumer(ctx, eventsStream, jetstream.ConsumerConfig{
		Durable:   consumerName,
		AckPolicy: jetstream.AckExplicitPolicy,
		// Not acknowledged within this (a worker died mid-event): delivered
		// again, to any worker.
		AckWait:       q.cfg.AckWait,
		MaxDeliver:    -1, // retries are counted by the worker (see Attempt), not capped here
		MaxAckPending: 4096,
	})
	if err != nil {
		return fmt.Errorf("create consumer: %w", err)
	}
	return nil
}

type message struct {
	ProjectID int64        `json:"project_id"`
	Event     *event.Event `json:"event"`
}

// Publish writes the batch and waits until JetStream has stored every
// event, so the 202 that follows is a promise. Publishes are sent together
// and awaited together: one round trip's worth of waiting, not one per event.
func (q *JetStream) Publish(ctx context.Context, events []*event.Event) error {
	futures := make([]jetstream.PubAckFuture, 0, len(events))
	for _, e := range events {
		data, err := json.Marshal(message{ProjectID: e.ProjectID, Event: e})
		if err != nil {
			return err
		}
		msg := &nats.Msg{Subject: "juju.events." + strconv.FormatInt(e.ProjectID, 10), Data: data, Header: nats.Header{}}
		msg.Header.Set(jetstream.MsgIDHeader, e.EventID)
		f, err := q.js.PublishMsgAsync(msg)
		if err != nil {
			return fmt.Errorf("publish: %w", err)
		}
		futures = append(futures, f)
	}
	for _, f := range futures {
		select {
		case <-f.Ok():
		case err := <-f.Err():
			if isFull(err) {
				return ErrFull
			}
			return fmt.Errorf("publish: %w", err)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// isFull recognises JetStream refusing a message because the stream is at
// its byte limit (DiscardNew).
func isFull(err error) bool {
	var apiErr *jetstream.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode == errCodeStoreFailed {
		return true
	}
	return err != nil && strings.Contains(err.Error(), "maximum bytes exceeded")
}

// Deliveries starts consuming on first call. Each process asks for at most
// Fetch messages at a time, so a slow worker doesn't hoard events another
// could store.
func (q *JetStream) Deliveries() <-chan Delivery {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.deliveries != nil {
		return q.deliveries
	}
	q.deliveries = make(chan Delivery, q.cfg.Fetch)
	cons, err := q.js.Consumer(context.Background(), eventsStream, consumerName)
	if err != nil {
		q.cfg.Log.Error("queue: open consumer", "err", err)
		close(q.deliveries)
		return q.deliveries
	}
	out := q.deliveries
	q.consume, err = cons.Consume(func(m jetstream.Msg) {
		var msg message
		if err := json.Unmarshal(m.Data(), &msg); err != nil || msg.Event == nil {
			// Can't be read, so can't ever be stored: set it aside.
			q.dead(m, "unreadable message: "+fmt.Sprint(err))
			_ = m.Term()
			return
		}
		e := msg.Event
		e.ProjectID = msg.ProjectID
		e.RestoreParsed() // fields JSON skips (parsed times), set by Normalize at ingest
		attempt := 1
		if md, err := m.Metadata(); err == nil {
			attempt = int(md.NumDelivered)
		}
		out <- &jsDelivery{q: q, m: m, e: e, attempt: attempt}
	}, jetstream.PullMaxMessages(q.cfg.Fetch))
	if err != nil {
		q.cfg.Log.Error("queue: consume", "err", err)
	}
	return out
}

// StopDeliveries stops taking new events and closes Deliveries. Events
// fetched but not yet given to a worker are handed back at once (nak), so
// another process can take them without waiting for AckWait.
func (q *JetStream) StopDeliveries() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.consume != nil {
		q.consume.Stop()
		<-q.consume.Closed()
		q.consume = nil
	}
	if q.deliveries != nil {
		close(q.deliveries)
	}
}

// Pending is how many events wait in the queue (for logs and health).
func (q *JetStream) Pending(ctx context.Context) (uint64, error) {
	s, err := q.js.Stream(ctx, eventsStream)
	if err != nil {
		return 0, err
	}
	info, err := s.Info(ctx)
	if err != nil {
		return 0, err
	}
	return info.State.Msgs, nil
}

// Conn is the NATS connection, shared with the live Bus.
func (q *JetStream) Conn() *nats.Conn { return q.nc }

func (q *JetStream) Close() {
	if q.nc != nil {
		_ = q.nc.Drain()
		q.nc.Close()
	}
	if q.ns != nil {
		q.ns.Shutdown()
		q.ns.WaitForShutdown()
	}
}

func (q *JetStream) dead(m jetstream.Msg, reason string) {
	msg := &nats.Msg{Subject: strings.Replace(m.Subject(), "juju.events.", "juju.dead.", 1), Data: m.Data(), Header: nats.Header{}}
	msg.Header.Set("Juju-Dead-Reason", reason)
	if _, err := q.js.PublishMsg(context.Background(), msg); err != nil {
		q.cfg.Log.Error("queue: could not keep a dead letter; it is lost", "reason", reason, "err", err)
	}
}

type jsDelivery struct {
	q       *JetStream
	m       jetstream.Msg
	e       *event.Event
	attempt int
}

func (d *jsDelivery) Event() *event.Event { return d.e }
func (d *jsDelivery) Attempt() int        { return d.attempt }
func (d *jsDelivery) Ack() {
	// DoubleAck waits for the server to confirm, so an ack lost on the way
	// doesn't make the event come back after it was stored. (If it does
	// anyway, the store drops the duplicate by event_id.)
	if err := d.m.DoubleAck(context.Background()); err != nil {
		d.q.cfg.Log.Warn("queue: ack failed; the event may be delivered again (and deduplicated)", "event_id", d.e.EventID, "err", err)
	}
}
func (d *jsDelivery) Retry(after time.Duration) { _ = d.m.NakWithDelay(after) }
func (d *jsDelivery) Dead(reason string) {
	d.q.dead(d.m, reason)
	_ = d.m.Term()
}
