// Package capacityrrd implements a multi-resolution round-robin database for
// storage capacity metrics (per-pool and per-dataset). Unlike the single-resolution
// rrd package, each tier stores min/avg/max aggregates per bucket.
//
// Three tiers:
//   - Tier 0: 5-minute samples,  2016 slots = 1 week
//   - Tier 1: 30-minute samples, 1440 slots = 30 days
//   - Tier 2: daily samples,     1825 slots = 5 years
package capacityrrd

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

const (
	Tier0Slots = 2016 // 7 * 24 * 12  (5-min resolution, 1 week)
	Tier1Slots = 1440 // 30 * 24 * 2  (30-min resolution, 30 days)
	Tier2Slots = 1825 // 365 * 5      (daily resolution, 5 years)

	tier0Secs = 5 * 60
	tier1Secs = 30 * 60
	tier2Secs = 24 * 60 * 60
)

// CapSample is one aggregated slot in a tier.
type CapSample struct {
	TS  int64   `json:"ts"`  // Unix epoch seconds (bucket start)
	Min float64 `json:"min"` // minimum value in bucket
	Avg float64 `json:"avg"` // mean value in bucket
	Max float64 `json:"max"` // maximum value in bucket
	N   int     `json:"n"`   // number of raw samples in bucket
}

// capSeries is a fixed-size circular buffer of CapSamples for one named series
// in one tier. We store it with a head index + count (same pattern as internal/rrd).
type capSeries struct {
	Buf   []CapSample `json:"buf"`
	Head  int         `json:"head"`
	Count int         `json:"count"`
	Slots int         `json:"slots"` // max capacity (constant per tier)
}

func newCapSeries(slots int) *capSeries {
	return &capSeries{
		Buf:   make([]CapSample, slots),
		Slots: slots,
	}
}

func (s *capSeries) push(sample CapSample) {
	s.Buf[s.Head] = sample
	s.Head = (s.Head + 1) % s.Slots
	if s.Count < s.Slots {
		s.Count++
	}
}

func (s *capSeries) all() []CapSample {
	if s.Count == 0 {
		return nil
	}
	result := make([]CapSample, s.Count)
	if s.Count < s.Slots {
		copy(result, s.Buf[:s.Count])
	} else {
		n := s.Slots - s.Head
		copy(result, s.Buf[s.Head:])
		copy(result[n:], s.Buf[:s.Head])
	}
	return result
}

// tierStore holds all series for one resolution tier.
type tierStore struct {
	Slots  int                    `json:"slots"`
	Series map[string]*capSeries  `json:"series"`
}

func newTierStore(slots int) tierStore {
	return tierStore{Slots: slots, Series: make(map[string]*capSeries)}
}

func (t *tierStore) get(key string) *capSeries {
	s, ok := t.Series[key]
	if !ok {
		s = newCapSeries(t.Slots)
		t.Series[key] = s
	}
	return s
}

// dbData is the full on-disk JSON structure.
type dbData struct {
	Tier0 tierStore `json:"tier0"`
	Tier1 tierStore `json:"tier1"`
	Tier2 tierStore `json:"tier2"`
}

// pendingBucket accumulates raw values within the current aggregate window.
type pendingBucket struct {
	bucketStart int64     // Unix seconds, truncated to bucket boundary
	pending     map[string][]float64
}

// DB is a thread-safe multi-resolution capacity round-robin database.
type DB struct {
	mu   sync.Mutex
	path string
	data dbData

	// In-memory accumulators for Tier1 (30-min) and Tier2 (daily) aggregation.
	t1 pendingBucket
	t2 pendingBucket
}

// Open loads a DB from disk, or creates a new empty one if the file doesn't exist.
func Open(path string) (*DB, error) {
	db := &DB{
		path: path,
		data: dbData{
			Tier0: newTierStore(Tier0Slots),
			Tier1: newTierStore(Tier1Slots),
			Tier2: newTierStore(Tier2Slots),
		},
		t1: pendingBucket{pending: make(map[string][]float64)},
		t2: pendingBucket{pending: make(map[string][]float64)},
	}

	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return db, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &db.data); err != nil {
		// Corrupt file — start fresh.
		db.data = dbData{
			Tier0: newTierStore(Tier0Slots),
			Tier1: newTierStore(Tier1Slots),
			Tier2: newTierStore(Tier2Slots),
		}
	}
	// Ensure Slots fields are correct after load (they may differ if we added tiers).
	db.data.Tier0.Slots = Tier0Slots
	db.data.Tier1.Slots = Tier1Slots
	db.data.Tier2.Slots = Tier2Slots
	for _, s := range db.data.Tier0.Series {
		s.Slots = Tier0Slots
		if len(s.Buf) != Tier0Slots {
			buf := make([]CapSample, Tier0Slots)
			copy(buf, s.Buf)
			s.Buf = buf
		}
	}
	for _, s := range db.data.Tier1.Series {
		s.Slots = Tier1Slots
		if len(s.Buf) != Tier1Slots {
			buf := make([]CapSample, Tier1Slots)
			copy(buf, s.Buf)
			s.Buf = buf
		}
	}
	for _, s := range db.data.Tier2.Series {
		s.Slots = Tier2Slots
		if len(s.Buf) != Tier2Slots {
			buf := make([]CapSample, Tier2Slots)
			copy(buf, s.Buf)
			s.Buf = buf
		}
	}
	return db, nil
}

// Record adds one 5-minute raw sample. It also folds pending values into
// Tier1 and Tier2 when bucket boundaries are crossed.
func (db *DB) Record(key string, value float64, now time.Time) {
	db.mu.Lock()
	defer db.mu.Unlock()

	// --- Tier 0: raw 5-min sample ---
	t0Bucket := now.Unix() / tier0Secs * tier0Secs
	db.data.Tier0.get(key).push(CapSample{TS: t0Bucket, Min: value, Avg: value, Max: value, N: 1})

	// --- Tier 1: 30-min aggregation ---
	t1Bucket := now.Unix() / tier1Secs * tier1Secs
	if db.t1.bucketStart == 0 {
		db.t1.bucketStart = t1Bucket
	}
	if t1Bucket != db.t1.bucketStart {
		// Bucket rolled — flush accumulated values into Tier1.
		db.flushTier1()
		db.t1.bucketStart = t1Bucket
	}
	db.t1.pending[key] = append(db.t1.pending[key], value)

	// --- Tier 2: daily aggregation ---
	t2Bucket := now.Unix() / tier2Secs * tier2Secs
	if db.t2.bucketStart == 0 {
		db.t2.bucketStart = t2Bucket
	}
	if t2Bucket != db.t2.bucketStart {
		db.flushTier2()
		db.t2.bucketStart = t2Bucket
	}
	db.t2.pending[key] = append(db.t2.pending[key], value)
}

// flushTier1 computes min/avg/max from pending and writes to Tier1. Must hold mu.
func (db *DB) flushTier1() {
	for key, vals := range db.t1.pending {
		if len(vals) == 0 {
			continue
		}
		s := aggregate(db.t1.bucketStart, vals)
		db.data.Tier1.get(key).push(s)
	}
	db.t1.pending = make(map[string][]float64)
}

// flushTier2 computes min/avg/max from pending and writes to Tier2. Must hold mu.
func (db *DB) flushTier2() {
	for key, vals := range db.t2.pending {
		if len(vals) == 0 {
			continue
		}
		s := aggregate(db.t2.bucketStart, vals)
		db.data.Tier2.get(key).push(s)
	}
	db.t2.pending = make(map[string][]float64)
}

// aggregate computes a CapSample from a slice of raw float64 values.
func aggregate(ts int64, vals []float64) CapSample {
	mn, mx, sum := vals[0], vals[0], 0.0
	for _, v := range vals {
		if v < mn {
			mn = v
		}
		if v > mx {
			mx = v
		}
		sum += v
	}
	return CapSample{TS: ts, Min: mn, Avg: sum / float64(len(vals)), Max: mx, N: len(vals)}
}

// Query returns all stored samples for the given tier and key, in chronological order.
// tier: 0 = 5-min, 1 = 30-min, 2 = daily.
// Returns nil if the series doesn't exist or is empty.
func (db *DB) Query(tier int, key string) []CapSample {
	db.mu.Lock()
	defer db.mu.Unlock()
	switch tier {
	case 0:
		s, ok := db.data.Tier0.Series[key]
		if !ok {
			return nil
		}
		return s.all()
	case 1:
		s, ok := db.data.Tier1.Series[key]
		if !ok {
			return nil
		}
		return s.all()
	case 2:
		s, ok := db.data.Tier2.Series[key]
		if !ok {
			return nil
		}
		return s.all()
	}
	return nil
}

// Keys returns all series names present in Tier0.
func (db *DB) Keys() []string {
	db.mu.Lock()
	defer db.mu.Unlock()
	keys := make([]string, 0, len(db.data.Tier0.Series))
	for k := range db.data.Tier0.Series {
		keys = append(keys, k)
	}
	return keys
}

// OldestTS returns the Unix timestamp of the oldest Tier0 sample across all series.
// Returns 0 if no data exists.
func (db *DB) OldestTS() int64 {
	db.mu.Lock()
	defer db.mu.Unlock()
	var oldest int64
	for _, s := range db.data.Tier0.Series {
		samples := s.all()
		if len(samples) == 0 {
			continue
		}
		ts := samples[0].TS
		if oldest == 0 || ts < oldest {
			oldest = ts
		}
	}
	return oldest
}

// DeleteKey removes all data for the given series across all tiers.
func (db *DB) DeleteKey(key string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	delete(db.data.Tier0.Series, key)
	delete(db.data.Tier1.Series, key)
	delete(db.data.Tier2.Series, key)
	delete(db.t1.pending, key)
	delete(db.t2.pending, key)
}

// Flush persists the DB to disk atomically.
func (db *DB) Flush() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	b, err := json.Marshal(&db.data)
	if err != nil {
		return err
	}
	return os.WriteFile(db.path, b, 0640)
}
