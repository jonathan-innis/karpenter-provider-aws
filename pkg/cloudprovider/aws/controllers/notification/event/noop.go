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

package event

import (
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type NoOp AWSMetadata

func (NoOp) EventID() string {
	return ""
}

func (NoOp) EC2InstanceIDs() []string {
	return []string{}
}

func (NoOp) Kind() Kind {
	return Kinds.NoOp
}

func (n NoOp) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	zap.Inline(AWSMetadata(n)).AddTo(enc)
	return nil
}

func (NoOp) StartTime() time.Time {
	return time.Now()
}
