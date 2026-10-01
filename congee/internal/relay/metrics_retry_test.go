package relay

import (
	"context"
	"errors"
	"github.com/michmich112/congee/internal/storage"
	"testing"
	"time"
)

type metricFailureStore struct {
	storage.Store
	fail   bool
	during func()
	bucket storage.RelayMetricBucket
}

func (s *metricFailureStore) UpsertRelayMetricBucket(_ context.Context, b storage.RelayMetricBucket) error {
	s.bucket = b
	if s.during != nil {
		s.during()
		s.during = nil
	}
	if s.fail {
		return errors.New("unavailable")
	}
	return nil
}
func TestMetricsFlushRetainsCountersOnFailure(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "minute", true: "shutdown"}[shutdown], func(t *testing.T) {
			m := newRelayMetrics()
			m.IncEventsStoredOK()
			m.IncEventsRejected()
			m.IncReq()
			m.IncClose()
			m.RecordQueryLatency(7 * time.Millisecond)
			st := &metricFailureStore{fail: true, during: func() { m.IncReq(); m.RecordQueryLatency(3 * time.Millisecond) }}
			flush := func() error {
				if shutdown {
					return m.FlushOpenMinute(context.Background(), st, nil)
				}
				return m.flushCompletedMinute(context.Background(), st, 60, nil)
			}
			if err := flush(); err == nil {
				t.Fatal("missing failure")
			}
			_, b := m.PartialMinuteBucket()
			if b.EventsStored != 1 || b.EventsRejected != 1 || b.ReqCount != 2 || b.CloseCount != 1 || b.QueryMsSum != 10 || b.QueryMsCount != 2 {
				t.Fatalf("lost activity: %+v", b)
			}
			st.fail = false
			if err := flush(); err != nil {
				t.Fatal(err)
			}
			if st.bucket.ReqCount != 2 || st.bucket.QueryMsSum != 10 {
				t.Fatal("retry lost activity")
			}
			_, b = m.PartialMinuteBucket()
			if b.ReqCount != 0 || b.QueryMsCount != 0 {
				t.Fatal("retry duplicated activity")
			}
		})
	}
}
