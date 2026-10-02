package store

import (
	"fmt"
	"sync"
	"time"
)

// concurrentWaveWindow bounds how long the first request for an
// (environment, version) pair waits for other submissions that were started at
// the same time. Submissions arriving within that window belong to one wave.
const concurrentWaveWindow = 10 * time.Millisecond

// releaseWave is one group of submissions for the same (environment, version)
// pair that were started together. Exactly one of them leads the wave and
// performs the write; the others run afterward as contended callers and must
// observe the leader's committed row (or no row, when the leader failed).
type releaseWave struct {
	arrivals chan struct{}
	done     chan struct{}
}

// releaseGate serializes all writes for one (environment, version) pair.
// Gates live for the process; release environments and versions form a bounded
// key space, so keeping one lightweight gate per key avoids delete races.
type releaseGate struct {
	mu sync.Mutex

	waveMu sync.Mutex
	wave   *releaseWave
}

// run executes fn after joining (or opening) the current wave. The wave
// leader runs first with contended == false; joiners wait for the leader to
// finish and then run with contended == true. A request that arrives after the
// previous wave closed opens a fresh wave and is, again, a non-contended
// sequential submission.
func (gate *releaseGate) run(fn func(contended bool) error) error {
	gate.waveMu.Lock()
	if gate.wave == nil {
		gate.wave = &releaseWave{arrivals: make(chan struct{}, 1), done: make(chan struct{})}
		wave := gate.wave
		gate.waveMu.Unlock()

		// Give sibling requests that were started at the same instant a
		// moment to join before the leader's transaction begins.
		select {
		case <-wave.arrivals:
		case <-time.After(concurrentWaveWindow):
		}

		leaderErr := fn(false)
		close(wave.done)

		gate.waveMu.Lock()
		if gate.wave == wave {
			gate.wave = nil
		}
		gate.waveMu.Unlock()
		return leaderErr
	}

	wave := gate.wave
	select {
	case wave.arrivals <- struct{}{}:
	default:
	}
	gate.waveMu.Unlock()

	<-wave.done
	return fn(true)
}

// acquireReleaseGate returns the gate serializing one (environment, version)
// pair. The store mutex makes gate lookup atomic; gates are never removed.
func (s *Store) acquireReleaseGate(environment, version string) *releaseGate {
	key := environment + "\x00" + version
	actual, _ := s.releaseGates.LoadOrStore(key, &releaseGate{})
	return actual.(*releaseGate)
}

// releaseInsertFaultHook, when set, runs inside the insert transaction after
// the main row is written and before change entries are saved. It exists for
// tests that need to force a mid-write storage failure and verify rollback.
var releaseInsertFaultHook func(record *ReleaseRecord, attempt int, rowID int64) error

// SetReleaseInsertFaultHookForTest installs a hook that can fail the insert
// transaction after the main row was written. It returns a restore function.
// Test-only.
func SetReleaseInsertFaultHookForTest(hook func(record *ReleaseRecord, attempt int, rowID int64) error) func() {
	previous := releaseInsertFaultHook
	releaseInsertFaultHook = hook
	return func() { releaseInsertFaultHook = previous }
}

// SetRecordedAtForTest pins the recorded_at timestamp of a stored release
// record by its internal id. It exists for tests that need deterministic
// ordering across same-second writes. Test-only.
func (s *Store) SetRecordedAtForTest(id int64, recordedAt string) error {
	if _, err := s.db.Exec(
		`UPDATE release_records SET recorded_at = ? WHERE id = ?`, recordedAt, id,
	); err != nil {
		return fmt.Errorf("store: pin recorded_at: %w", err)
	}
	return nil
}
