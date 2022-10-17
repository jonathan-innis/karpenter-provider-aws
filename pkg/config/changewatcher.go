/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package config

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"sync"

	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/configmap/informer"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/system"

	"github.com/aws/karpenter/pkg/utils/atomic"
)

type ChangeHandler func(context.Context, *v1.ConfigMap)

type ChangeWatcher struct {
	ctx context.Context
	mu  sync.RWMutex

	// hash of the config map, so we only notify watches if it has changed
	configHash uint64
	cached     *v1.ConfigMap // cached is the internal representation of the configMap for hydration

	watchers atomic.Slice[ChangeHandler]
}

func NewChangeWatcher(ctx context.Context, iw *informer.InformedWatcher) (*ChangeWatcher, error) {
	if iw.Namespace != system.Namespace() {
		return nil, fmt.Errorf("watcher configured for wrong namespace, expected %s found %s", system.Namespace(), iw.Namespace)
	}

	cw := &ChangeWatcher{
		ctx:    ctx,
		cached: &v1.ConfigMap{},
	}
	logging.FromContext(ctx).Infof("loading config from %s/%s", system.Namespace(), configMapName)

	iw.Watch(configMapName, cw.configMapChanged)
	return &ChangeWatcher{}, nil
}

func (cw *ChangeWatcher) OnChange(handler ChangeHandler) {
	cw.watchers.Add(handler)

	// Perform initial hydration in case OnChange was added after the informer was started
	cw.mu.Lock()
	defer cw.mu.Unlock()
	handler(cw.ctx, cw.cached)
}

func (cw *ChangeWatcher) configMapChanged(configMap *v1.ConfigMap) {
	cw.mu.Lock()
	defer cw.mu.Unlock()

	hash := hashCM(configMap)
	if hash == cw.configHash {
		return
	}
	if cw.configHash != 0 {
		logging.FromContext(cw.ctx).Infof("configuration change detected")
	}
	cw.cached = configMap.DeepCopy()
	cw.configHash = hash

	// notify watchers
	cw.watchers.Range(func(w ChangeHandler) bool {
		w(cw.ctx, configMap.DeepCopy())
		return true
	})
}

// hashCM hashes a
func hashCM(cm *v1.ConfigMap) uint64 {
	var keys []string
	for k := range cm.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	hasher := fnv.New64()
	for _, k := range keys {
		fmt.Fprint(hasher, k)
		fmt.Fprint(hasher, cm.Data[k])
	}

	keys = nil
	for k := range cm.BinaryData {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprint(hasher, k)
		hasher.Write(cm.BinaryData[k])
	}
	return hasher.Sum64()
}
