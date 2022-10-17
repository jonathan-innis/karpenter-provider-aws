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
	"sync"
	"time"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/logging"
)

const (
	configMapName = "karpenter-global-settings"
)

type Config interface {
	// BatchMaxDuration returns the maximum batch duration
	BatchMaxDuration() time.Duration
	// BatchIdleDuration returns the maximum idle period used to extend a batch duration up to BatchMaxDuration
	BatchIdleDuration() time.Duration
}

type config struct {
	mu  sync.RWMutex
	ctx context.Context

	batchMaxDuration  time.Duration
	batchIdleDuration time.Duration
}

func (c *config) BatchMaxDuration() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.batchMaxDuration
}

func (c *config) BatchIdleDuration() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.batchIdleDuration
}

const (
	paramBatchMaxDuration  = "batchMaxDuration"
	paramBatchIdleDuration = "batchIdleDuration"
)

// these values need to be synced with our templates/configmap.yaml
var defaultConfigMapData = map[string]string{
	paramBatchMaxDuration:  "10s",
	paramBatchIdleDuration: "1s",
}

func NewConfig(cw *ChangeWatcher) Config {
	cfg := &config{}
	cw.OnChange(cfg.changeHandler)
	return cfg
}

func (c *config) changeHandler(ctx context.Context, cm *v1.ConfigMap) {
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data = lo.Assign(cm.Data, defaultConfigMapData)

	c.mu.Lock()
	for k, v := range cm.Data {
		switch k {
		case paramBatchMaxDuration:
			c.batchMaxDuration = c.parsePositiveDuration(k, v, defaultConfigMapData[k])
		case paramBatchIdleDuration:
			c.batchIdleDuration = c.parsePositiveDuration(k, v, defaultConfigMapData[k])
		default:
			logging.FromContext(c.ctx).Warnf("ignoring unknown config parameter %s", k)
		}
	}
	c.mu.Unlock()
}

func (c *config) parsePositiveDuration(configKey, configValue string, defaultValue string) time.Duration {
	duration, err := time.ParseDuration(configValue)
	if err != nil {
		logging.FromContext(c.ctx).Errorf("unable to parse %s value %q: %s, using default value of %s", configKey, configValue, err, defaultValue)
	} else if duration < 0 {
		logging.FromContext(c.ctx).Errorf("negative values not allowed for %s, using default value of %s", configKey, defaultValue)
		duration = 0
	}
	if duration == 0 {
		duration, err = time.ParseDuration(defaultValue)
		if err != nil {
			// shouldn't occur, but just in case
			logging.FromContext(c.ctx).Errorf("parsing default value %s for key %s, %s", configValue, configKey, err)
		}
	}
	return duration
}
