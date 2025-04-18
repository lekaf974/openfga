package storagewrappers

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"

	"github.com/openfga/openfga/internal/build"
	"github.com/openfga/openfga/pkg/storage"
	"github.com/openfga/openfga/pkg/telemetry"
)

const timeWaitingSpanAttribute = "time_waiting"

var _ storage.RelationshipTupleReader = (*BoundedConcurrencyTupleReader)(nil)

var (
	boundedReadDelayMsHistogram = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:                       build.ProjectName,
		Name:                            "datastore_bounded_read_delay_ms",
		Help:                            "Time spent waiting for Read, ReadUserTuple and ReadUsersetTuples calls to the datastore",
		Buckets:                         []float64{1, 3, 5, 10, 25, 50, 100, 1000, 5000}, // Milliseconds. Upper bound is config.UpstreamTimeout.
		NativeHistogramBucketFactor:     1.1,
		NativeHistogramMaxBucketNumber:  100,
		NativeHistogramMinResetDuration: time.Hour,
	}, []string{"grpc_service", "grpc_method"})
)

type BoundedConcurrencyTupleReader struct {
	storage.RelationshipTupleReader
	limiter chan struct{}
}

// NewBoundedConcurrencyTupleReader returns a wrapper over a datastore that makes sure that there are, at most,
// "concurrency" concurrent calls to Read, ReadUserTuple and ReadUsersetTuples.
// Consumers can then rest assured that one client will not hoard all the database connections available.
func NewBoundedConcurrencyTupleReader(wrapped storage.RelationshipTupleReader, concurrency uint32) *BoundedConcurrencyTupleReader {
	return &BoundedConcurrencyTupleReader{
		RelationshipTupleReader: wrapped,
		limiter:                 make(chan struct{}, concurrency),
	}
}

// ReadUserTuple tries to return one tuple that matches the provided key exactly.
func (b *BoundedConcurrencyTupleReader) ReadUserTuple(
	ctx context.Context,
	store string,
	tupleKey *openfgav1.TupleKey,
	options storage.ReadUserTupleOptions,
) (*openfgav1.Tuple, error) {
	err := b.waitForLimiter(ctx)
	if err != nil {
		return nil, err
	}

	defer func() {
		<-b.limiter
	}()

	return b.RelationshipTupleReader.ReadUserTuple(ctx, store, tupleKey, options)
}

// Read the set of tuples associated with `store` and `TupleKey`, which may be nil or partially filled.
func (b *BoundedConcurrencyTupleReader) Read(ctx context.Context, store string, tupleKey *openfgav1.TupleKey, options storage.ReadOptions) (storage.TupleIterator, error) {
	err := b.waitForLimiter(ctx)
	if err != nil {
		return nil, err
	}

	defer func() {
		<-b.limiter
	}()

	return b.RelationshipTupleReader.Read(ctx, store, tupleKey, options)
}

// ReadUsersetTuples returns all userset tuples for a specified object and relation.
func (b *BoundedConcurrencyTupleReader) ReadUsersetTuples(
	ctx context.Context,
	store string,
	filter storage.ReadUsersetTuplesFilter,
	options storage.ReadUsersetTuplesOptions,
) (storage.TupleIterator, error) {
	err := b.waitForLimiter(ctx)
	if err != nil {
		return nil, err
	}

	defer func() {
		<-b.limiter
	}()

	return b.RelationshipTupleReader.ReadUsersetTuples(ctx, store, filter, options)
}

// ReadStartingWithUser performs a reverse read of relationship tuples starting at one or
// more user(s) or userset(s) and filtered by object type and relation.
func (b *BoundedConcurrencyTupleReader) ReadStartingWithUser(
	ctx context.Context,
	store string,
	filter storage.ReadStartingWithUserFilter,
	options storage.ReadStartingWithUserOptions,
) (storage.TupleIterator, error) {
	err := b.waitForLimiter(ctx)
	if err != nil {
		return nil, err
	}

	defer func() {
		<-b.limiter
	}()

	return b.RelationshipTupleReader.ReadStartingWithUser(ctx, store, filter, options)
}

// waitForLimiter respects context errors and returns an error only if it couldn't send an item to the channel.
func (b *BoundedConcurrencyTupleReader) waitForLimiter(ctx context.Context) error {
	start := time.Now()
	defer func() {
		timeWaiting := time.Since(start).Milliseconds()

		rpcInfo := telemetry.RPCInfoFromContext(ctx)
		boundedReadDelayMsHistogram.WithLabelValues(
			rpcInfo.Service,
			rpcInfo.Method,
		).Observe(float64(timeWaiting))

		span := trace.SpanFromContext(ctx)
		span.SetAttributes(attribute.Int64(timeWaitingSpanAttribute, timeWaiting))
	}()

	select {
	// Note: if both cases can proceed, one will be selected at random
	case <-ctx.Done():
		return ctx.Err()
	case b.limiter <- struct{}{}:
		break
	}

	return nil
}
