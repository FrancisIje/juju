package live

import (
	"log/slog"
	"strconv"
	"strings"

	"github.com/nats-io/nats.go"
)

// Bus carries "project N changed" between processes over NATS, so a worker
// storing an event refreshes dashboards connected to any API process, not
// only one in the same process. It's plain NATS publish/subscribe, not
// JetStream: a refresh signal that nobody hears is fine to lose.
type Bus struct {
	NC  *nats.Conn
	Log *slog.Logger
}

const subjectPrefix = "juju.live."

// Changed announces a change. Fire and forget: it never blocks the worker.
func (b *Bus) Changed(projectID int64) {
	if err := b.NC.Publish(subjectPrefix+strconv.FormatInt(projectID, 10), nil); err != nil && b.Log != nil {
		b.Log.Debug("live: publish", "err", err)
	}
}

// Deliver passes every change announced on the bus to hub, which signals
// the dashboards connected to this process. Returns the subscription.
func (b *Bus) Deliver(hub *Hub) (*nats.Subscription, error) {
	return b.NC.Subscribe(subjectPrefix+"*", func(m *nats.Msg) {
		if id, err := strconv.ParseInt(strings.TrimPrefix(m.Subject, subjectPrefix), 10, 64); err == nil {
			hub.Changed(id)
		}
	})
}
