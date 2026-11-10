package job

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	bolt "go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"
)

// ErrBusy means another run holds the store (the store doubles as the single-instance lock).
var ErrBusy = errors.New("another run is using this folder")

// Store is the bbolt file that makes every action resumable.
type Store struct {
	db        *bolt.DB
	recovered bool
}

// OpenStore opens or creates the store. A corrupt file is kept as path.corrupt-<stamp> and a
// fresh store is created; Recovered then reports true so the caller can rebuild (R18).
func OpenStore(path string) (*Store, error) {
	db, err := openBolt(path)
	if err == nil {
		return &Store{db: db}, nil
	}
	if errors.Is(err, ErrBusy) || !isCorrupt(err) {
		return nil, err
	}
	// The store lives in our own Reports folder on the destination; the damaged copy is kept
	// for inspection rather than deleted.
	aside := fmt.Sprintf("%s.corrupt-%s", path, time.Now().Format("20060102_150405.000"))
	if rerr := os.Rename(path, aside); rerr != nil {
		return nil, fmt.Errorf("store damaged (%v) and could not be moved aside: %w", err, rerr)
	}
	db, err = openBolt(path)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, recovered: true}, nil
}

type corruptErr struct{ cause any }

func (c corruptErr) Error() string { return fmt.Sprintf("store damaged: %v", c.cause) }

func isCorrupt(err error) bool {
	var c corruptErr
	return errors.As(err, &c) || errors.Is(err, berrors.ErrInvalid) ||
		errors.Is(err, berrors.ErrChecksum) || errors.Is(err, berrors.ErrVersionMismatch)
}

func openBolt(path string) (db *bolt.DB, err error) {
	defer func() {
		if r := recover(); r != nil {
			db, err = nil, corruptErr{r}
		}
	}()
	db, err = bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if errors.Is(err, berrors.ErrTimeout) {
		return nil, ErrBusy
	}
	return db, err
}

// Recovered reports that the previous store was damaged and this one starts empty.
func (s *Store) Recovered() bool { return s.recovered }

// Close releases the store and its lock.
func (s *Store) Close() error { return s.db.Close() }

// Path is the store's file.
func (s *Store) Path() string { return s.db.Path() }

func guard(err *error) {
	if r := recover(); r != nil {
		*err = corruptErr{r}
	}
}

// Put stores v as JSON under bucket/key in one fsync'd transaction.
func Put[T any](s *Store, bucket, key string, v T) (err error) {
	defer guard(&err)
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		bk, err := tx.CreateBucketIfNotExists([]byte(bucket))
		if err != nil {
			return err
		}
		return bk.Put([]byte(key), b)
	})
}

// Get loads bucket/key; ok is false when absent.
func Get[T any](s *Store, bucket, key string) (v T, ok bool, err error) {
	defer guard(&err)
	err = s.db.View(func(tx *bolt.Tx) error {
		bk := tx.Bucket([]byte(bucket))
		if bk == nil {
			return nil
		}
		raw := bk.Get([]byte(key))
		if raw == nil {
			return nil
		}
		ok = true
		return json.Unmarshal(raw, &v)
	})
	return v, ok, err
}

// ForEach visits bucket in key order. fn must not write to the store.
func ForEach[T any](s *Store, bucket string, fn func(key string, v T) error) (err error) {
	defer guard(&err)
	return s.db.View(func(tx *bolt.Tx) error {
		bk := tx.Bucket([]byte(bucket))
		if bk == nil {
			return nil
		}
		return bk.ForEach(func(k, raw []byte) error {
			var v T
			if err := json.Unmarshal(raw, &v); err != nil {
				return err
			}
			return fn(string(k), v)
		})
	})
}

// Delete removes bucket/key (absent is fine).
func Delete(s *Store, bucket, key string) (err error) {
	defer guard(&err)
	return s.db.Update(func(tx *bolt.Tx) error {
		if bk := tx.Bucket([]byte(bucket)); bk != nil {
			return bk.Delete([]byte(key))
		}
		return nil
	})
}

// ClearBucket removes every key in bucket.
func ClearBucket(s *Store, bucket string) (err error) {
	defer guard(&err)
	return s.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(bucket)) == nil {
			return nil
		}
		return tx.DeleteBucket([]byte(bucket))
	})
}
