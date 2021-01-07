// Copyright 2019 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package ledger implements a modified map with three unique characteristics:
// 1. every unique state of the map is given a unique hash
// 2. prior states of the map are retained for a fixed period of time
// 2. given a previous hash, we can retrieve a previous state from the map, if it is still retained.
package ledger

import (
	"encoding/base64"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/spaolacci/murmur3"

	"istio.io/pkg/cache"
)

// Ledger exposes a modified map with three unique characteristics:
// 1. every unique state of the map is given a unique hash
// 2. prior states of the map are retained until erased
// 3. given a previous hash, we can retrieve a previous state from the map, if it is still retained.
type Ledger interface {
	// Put adds or overwrites a key in the Ledger
	Put(key, value string) (string, error)
	// Delete removes a key from the Ledger, which may still be read using GetPreviousValue
	Delete(key string) (string, error)
	// Get returns a the value of the key from the Ledger's current state
	Get(key string) (string, error)
	// RootHash is the hash of all keys and values currently in the Ledger
	RootHash() string
	// GetPreviousValue executes a get against a previous version of the ledger, using that version's root hash.
	GetPreviousValue(previousRootHash, key string) (result string, err error)
	// EraseRootHash re-claims any memory used by this version of history, preserving bits shared with other versions.
	EraseRootHash(rootHash string) error
	// Stats gives basic storage info regarding the ledger's underlying cache
	Stats() cache.Stats
	// GetAll returns the entire state of the ledger
	GetAll() (map[string]string, error)
	// GetAllPrevious returns the entire state of the ledger at an arbitrary version
	GetAllPrevious(string) (map[string]string, error)
}

type smtLedger struct {
	tree *smt
	// history tracks the sequence of versions of the ledger for use while erasing
	history *history
	// keys in smt are hashed.  this cache allows us to reverse the hash and reconstruct the keys
	keyCache      byteCache
	firstObserved map[string][]byte
	eraselock     sync.Mutex
}

func Make() Ledger {
	return &gcledger{
		inner: &smtLedger{
			tree:    newSMT(hasher, nil),
			history: newHistory(),
			// keyCache should have ~512kB memory max, each entry is 128 bits = 2^23/2^7 = 2^16
			keyCache:      byteCache{cache: cache.NewLRU(forever, time.Minute, math.MaxUint16)},
			firstObserved: make(map[string][]byte),
		},
	}
}

// makeOld returns a Ledger which will retain previous nodes after they are deleted.
// the retention parameter has been removed in favor of EraseRootHash, but is left
// here for backwards compatibility
func makeOld(_ time.Duration) Ledger {
	return &smtLedger{
		tree:    newSMT(hasher, nil),
		history: newHistory(),
		// keyCache should have ~512kB memory max, each entry is 128 bits = 2^23/2^7 = 2^16
		keyCache:      byteCache{cache: cache.NewLRU(forever, time.Minute, math.MaxUint16)},
		firstObserved: make(map[string][]byte),
	}
}

func (s *smtLedger) EraseRootHash(rootHash string) error {
	s.eraselock.Lock()
	defer s.eraselock.Unlock()
	// occurrences is a list of every time in (unerased) history when this hash has been observed
	occurrences := s.history.Get(rootHash)
	if len(occurrences) == 0 {
		return fmt.Errorf("rootHash %s is not present in ledger history", rootHash)
	}
	var adjacentRoots [][]byte
	for _, o := range occurrences {
		if o.Next() == nil {
			return fmt.Errorf("cannot erase current rootHash")
		}
		var prevVal []byte
		if o.Prev() != nil {
			prevVal = o.Prev().Value.([]byte)
		}
		adjacentRoots = append(adjacentRoots, prevVal, o.Next().Value.([]byte))
	}
	err := s.tree.Erase(occurrences[0].Value.([]byte), adjacentRoots)
	if err != nil {
		return err
	}
	s.history.lock.Lock()
	for _, o := range occurrences {
		s.history.Remove(o)
	}
	s.history.lock.Unlock()
	s.history.Delete(rootHash)
	return nil
}

// Put adds a key value pair to the ledger, overwriting previous values and marking them for
// removal after the retention specified in makeOld().  The implementation of Erase depends on
// the value for each key never regressing to old states.
func (s *smtLedger) Put(key, value string) (result string, err error) {
	b, err := s.tree.Update([][]byte{s.coerceKeyToHashLen(key)}, [][]byte{stringToBytes(value)})
	if err != nil {
		return
	}
	_, result = s.history.Put(b)
	return
}

// Delete removes a key value pair from the ledger, marking it for removal after the retention specified in makeOld()
func (s *smtLedger) Delete(key string) (string, error) {
	// deletes are the only case where a tree or sub-tree can revert to a previous state.
	b, err := s.tree.Delete(s.coerceKeyToHashLen(key))
	if err != nil {
		return "", err
	}
	_, res := s.history.Put(b)
	return res, nil
}

// GetPreviousValue returns the value of key when the ledger's RootHash was previousHash, if it is still retained.
func (s *smtLedger) GetPreviousValue(previousRootHash, key string) (result string, err error) {
	prevBytes, err := base64.StdEncoding.DecodeString(previousRootHash)
	if err != nil {
		return "", err
	}
	b, err := s.tree.GetPreviousValue(prevBytes, s.coerceKeyToHashLen(key))
	result = string(trimLeadingZeroes(b))
	return
}

// Get returns the current value of key.
func (s *smtLedger) Get(key string) (result string, err error) {
	return s.GetPreviousValue(s.RootHash(), key)
}

// RootHash represents the hash of the current state of the ledger.
func (s *smtLedger) RootHash() string {
	return hashToString(s.tree.Root())
}

func hashToString(h []byte) string {
	return base64.StdEncoding.EncodeToString(h)
}

func (s *smtLedger) coerceKeyToHashLen(val string) []byte {
	hasher := murmur3.New64()
	_, _ = hasher.Write([]byte(val))
	result := hasher.Sum(nil)
	var h hash
	copy(h[:], result)
	s.keyCache.Set(h, [][]byte{stringToBytes(val)})
	return result
}

func stringToBytes(val string) []byte {
	return []byte(val)
}

func trimLeadingZeroes(in []byte) []byte {
	var i int
	for i = range in {
		if in[i] != 0 {
			break
		}
	}
	return in[i:]
}

func (s *smtLedger) Stats() cache.Stats {
	return s.tree.Stats()
}

func (s *smtLedger) GetAll() (map[string]string, error) {
	return s.GetAllPrevious(s.RootHash())
}

func (s *smtLedger) GetAllPrevious(prevRoot string) (map[string]string, error) {
	prevBytes, err := base64.StdEncoding.DecodeString(prevRoot)
	if err != nil {
		return nil, err
	}
	keys, values, err := s.tree.GetAllPrevious(prevBytes)
	result := make(map[string]string)
	for i := range keys {
		var h hash
		copy(h[:], keys[i])
		if truekey, ok := s.keyCache.Get(h); ok {
			result[string(trimLeadingZeroes(truekey[0]))] = string(trimLeadingZeroes(values[i]))
		} else {
			err = multierror.Append(err, fmt.Errorf("could not find original value for key %x", keys[i]))
		}
	}
	return result, err
}
