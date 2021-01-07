// Copyright Istio Authors
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

package ledger

import (
	"container/list"
	"sync"
)

type history struct {
	*list.List
	index map[string][]*list.Element

	// lock is for the whole struct
	lock sync.RWMutex
}

func newHistory() *history {
	return &history{
		List:  list.New(),
		index: make(map[string][]*list.Element),
	}
}

func (h *history) Get(hash string) []*list.Element {
	h.lock.RLock()
	defer h.lock.RUnlock()
	return h.index[hash]
}

func (h *history) Put(key []byte) (*list.Element, string) {
	h.lock.Lock()
	defer h.lock.Unlock()
	result := h.PushBack(key)
	encodedKey := hashToString(key)
	h.index[encodedKey] = append(h.index[encodedKey], result)
	return result, encodedKey
}

func (h *history) Delete(key string) {
	h.lock.Lock()
	defer h.lock.Unlock()
	delete(h.index, key)
}
