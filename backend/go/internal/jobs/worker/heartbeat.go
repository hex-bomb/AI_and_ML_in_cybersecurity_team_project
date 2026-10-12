package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kbsearch/kb/internal/envelope"
)

// heartbeat — продление lease на время обработки (тип ради видимых начала и конца).
type heartbeat struct {
	stop func()
	lost func() bool
}

// startHeartbeat запускает продление lease в фоне (тикер — на контексте задания).
func (w *Worker) startHeartbeat(
	ctx context.Context, cancelJob context.CancelFunc, lease domain.JobLease,
) *heartbeat {
	var lost atomic.Bool

	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(w.opts.HeartbeatInterval)
		defer ticker.Stop()

		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				ok, err := w.deps.Jobs.Heartbeat(
					ctx, w.deps.Queryer, lease.JobID.String(), lease.LeaseToken,
				)
				if err != nil {
					// Сбой продления — не потеря lease, ждём следующий тик.
					w.log.Warn("продление lease не выполнено", "job", lease.JobID, "err", err)
					continue
				}
				if ok {
					continue
				}
				// Lease перехвачен — продолжать нельзя (ТЗ 7.7).
				lost.Store(true)
				cancelJob()
				return
			}
		}
	}()

	var once sync.Once
	return &heartbeat{
		stop: func() {
			once.Do(func() {
				close(stop)
				<-done
			})
		},
		lost: lost.Load,
	}
}
