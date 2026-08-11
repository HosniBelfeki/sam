// Copyright 2026 Google LLC
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

package discovery

import (
	"sync"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
)

// topicManager refcounts pubsub topic handles: gossipsub allows a single
// Join per topic per host, and the announcer and the view may need the
// same topic (a node can provide and consume the same key).
type topicManager struct {
	ps     *pubsub.PubSub
	mu     sync.Mutex
	topics map[string]*topicRef
}

type topicRef struct {
	topic *pubsub.Topic
	refs  int
}

func newTopicManager(ps *pubsub.PubSub) *topicManager {
	return &topicManager{ps: ps, topics: map[string]*topicRef{}}
}

// acquire joins the topic (or reuses the existing handle) and returns a
// release func. The topic is closed when the last holder releases it.
func (tm *topicManager) acquire(name string) (*pubsub.Topic, func(), error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	ref, ok := tm.topics[name]
	if !ok {
		topic, err := tm.ps.Join(name)
		if err != nil {
			return nil, nil, err
		}
		ref = &topicRef{topic: topic}
		tm.topics[name] = ref
	}
	ref.refs++
	var once sync.Once
	release := func() {
		once.Do(func() {
			tm.mu.Lock()
			defer tm.mu.Unlock()
			ref.refs--
			if ref.refs == 0 {
				if err := ref.topic.Close(); err != nil {
					logger.Debugf("[Discovery] closing topic %s: %v", name, err)
				}
				delete(tm.topics, name)
			}
		})
	}
	return ref.topic, release, nil
}
