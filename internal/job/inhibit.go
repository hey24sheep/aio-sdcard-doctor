package job

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"sync/atomic"
)

var (
	inhibitImpl   = inhibitOS
	inhibitWarned atomic.Bool
)

// Inhibit keeps the computer from sleeping while an action runs. When the OS mechanism is
// unavailable the error is returned once per process (the caller shows it as a warning) and
// later calls succeed silently. The returned release is idempotent.
func Inhibit(reason string) (release func(), err error) {
	rel, err := inhibitImpl(reason)
	if err != nil {
		if inhibitWarned.Swap(true) {
			return func() {}, nil
		}
		return func() {}, err
	}
	var once sync.Once
	return func() { once.Do(rel) }, nil
}

// ErrSourceBusy means another window or process is already reading this card (R14).
var ErrSourceBusy = errors.New("this card is already being read by another window")

// LockSource takes the per-card lock before any read. key must survive a re-plug
// (DeviceSerial + Size + FSSerial). The returned unlock is idempotent.
func LockSource(key string) (unlock func(), err error) {
	sum := sha256.Sum256([]byte(key))
	rel, err := lockSourceOS(hex.EncodeToString(sum[:8]))
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(rel) }, nil
}
