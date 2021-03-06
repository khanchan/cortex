package ingester

import (
	"sync"

	"github.com/prometheus/common/model"

	"github.com/prometheus/prometheus/storage/local/chunk"
)

// fingerprintSeriesPair pairs a fingerprint with a memorySeries pointer.
type fingerprintSeriesPair struct {
	fp     model.Fingerprint
	series *memorySeries
}

// seriesMap maps fingerprints to memory series. All its methods are
// goroutine-safe. A seriesMap is effectively is a goroutine-safe version of
// map[model.Fingerprint]*memorySeries.
type seriesMap struct {
	mtx sync.RWMutex
	m   map[model.Fingerprint]*memorySeries
}

// newSeriesMap returns a newly allocated empty seriesMap. To create a seriesMap
// based on a prefilled map, use an explicit initializer.
func newSeriesMap() *seriesMap {
	return &seriesMap{m: make(map[model.Fingerprint]*memorySeries)}
}

// length returns the number of mappings in the seriesMap.
func (sm *seriesMap) length() int {
	sm.mtx.RLock()
	defer sm.mtx.RUnlock()

	return len(sm.m)
}

// get returns a memorySeries for a fingerprint. Return values have the same
// semantics as the native Go map.
func (sm *seriesMap) get(fp model.Fingerprint) (s *memorySeries, ok bool) {
	sm.mtx.RLock()
	s, ok = sm.m[fp]
	// Note that the RUnlock is not done via defer for performance reasons.
	// TODO(beorn7): Once https://github.com/golang/go/issues/14939 is
	// fixed, revert to the usual defer idiom.
	sm.mtx.RUnlock()
	return
}

// put adds a mapping to the seriesMap. It panics if s == nil.
func (sm *seriesMap) put(fp model.Fingerprint, s *memorySeries) {
	sm.mtx.Lock()
	defer sm.mtx.Unlock()

	if s == nil {
		panic("tried to add nil pointer to seriesMap")
	}
	sm.m[fp] = s
}

// del removes a mapping from the series Map.
func (sm *seriesMap) del(fp model.Fingerprint) {
	sm.mtx.Lock()
	defer sm.mtx.Unlock()

	delete(sm.m, fp)
}

// iter returns a channel that produces all mappings in the seriesMap. The
// channel will be closed once all fingerprints have been received. Not
// consuming all fingerprints from the channel will leak a goroutine. The
// semantics of concurrent modification of seriesMap is the similar as the one
// for iterating over a map with a 'range' clause. However, if the next element
// in iteration order is removed after the current element has been received
// from the channel, it will still be produced by the channel.
func (sm *seriesMap) iter() <-chan fingerprintSeriesPair {
	ch := make(chan fingerprintSeriesPair)
	go func() {
		sm.mtx.RLock()
		for fp, s := range sm.m {
			sm.mtx.RUnlock()
			ch <- fingerprintSeriesPair{fp, s}
			sm.mtx.RLock()
		}
		sm.mtx.RUnlock()
		close(ch)
	}()
	return ch
}

type memorySeries struct {
	metric model.Metric
	// Sorted by start time, overlapping chunk ranges are forbidden.
	chunkDescs []*chunk.Desc
	// The timestamp of the last sample in this series. Needed to
	// ensure timestamp monotonicity during ingestion.
	lastTime model.Time
	// The value of the last sample in this series. Needed to
	// ensure timestamp monotonicity during ingestion.
	lastSampleValue model.SampleValue
	// Whether lastSampleValue has been set already.
	lastSampleValueSet bool
	// Whether the current head chunk has already been finished.  If true,
	// the current head chunk must not be modified anymore.
	headChunkClosed bool
}

// newMemorySeries returns a pointer to a newly allocated memorySeries for the
// given metric.
func newMemorySeries(m model.Metric) *memorySeries {
	return &memorySeries{
		metric:   m,
		lastTime: model.Earliest,
	}
}

// add adds a sample pair to the series. It returns the number of newly
// completed chunks (which are now eligible for persistence).
//
// The caller must have locked the fingerprint of the series.
func (s *memorySeries) add(v model.SamplePair) (int, error) {
	if len(s.chunkDescs) == 0 || s.headChunkClosed {
		newHead := chunk.NewDesc(chunk.New(), v.Timestamp)
		s.chunkDescs = append(s.chunkDescs, newHead)
		s.headChunkClosed = false
	}

	chunks, err := s.head().Add(v)
	if err != nil {
		return 0, err
	}
	s.head().C = chunks[0]

	for _, c := range chunks[1:] {
		s.chunkDescs = append(s.chunkDescs, chunk.NewDesc(c, c.FirstTime()))
	}

	// Populate lastTime of now-closed chunks.
	for _, cd := range s.chunkDescs[len(s.chunkDescs)-len(chunks) : len(s.chunkDescs)-1] {
		cd.MaybePopulateLastTime()
	}

	s.lastTime = v.Timestamp
	s.lastSampleValue = v.Value
	s.lastSampleValueSet = true
	return len(chunks) - 1, nil
}

func (s *memorySeries) closeHead() {
	s.headChunkClosed = true
	s.head().MaybePopulateLastTime()
}

// head returns a pointer to the head chunk descriptor. The caller must have
// locked the fingerprint of the memorySeries. This method will panic if this
// series has no chunk descriptors.
func (s *memorySeries) head() *chunk.Desc {
	return s.chunkDescs[len(s.chunkDescs)-1]
}
