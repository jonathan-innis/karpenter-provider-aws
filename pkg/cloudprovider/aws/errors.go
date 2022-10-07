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

package aws

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/samber/lo"
)

const (
	launchTemplateNotFoundCode = "InvalidLaunchTemplateName.NotFoundException"
	AccessDeniedCode           = "AccessDenied"
	AccessDeniedException      = "AccessDeniedException"
)

var (
	// This is not an exhaustive list, add to it as needed
	notFoundErrorCodes = []string{
		"InvalidInstanceID.NotFound",
		launchTemplateNotFoundCode,
	}
	// unfulfillableCapacityErrorCodes signify that capacity is temporarily unable to be launched
	unfulfillableCapacityErrorCodes = []string{
		"InsufficientInstanceCapacity",
		"MaxSpotInstanceCountExceeded",
		"VcpuLimitExceeded",
		"UnfulfillableCapacity",
		"Unsupported",
	}
)

type InstanceTerminatedError struct {
	error
}

func isInstanceTerminated(err error) bool {
	if err == nil {
		return false
	}
	var itErr InstanceTerminatedError
	return errors.As(err, &itErr)
}

// isNotFound returns true if the err is an AWS error (even if it's
// wrapped) and is a known to mean "not found" (as opposed to a more
// serious or unexpected error)
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var awsError awserr.Error
	if errors.As(err, &awsError) {
		return lo.Contains(notFoundErrorCodes, awsError.Code())
	}
	return false
}

// isUnfulfillableCapacity returns true if the Fleet err means
// capacity is temporarily unavailable for launching.
// This could be due to account limits, insufficient ec2 capacity, etc.
func isUnfulfillableCapacity(err *ec2.CreateFleetError) bool {
	return lo.Contains(unfulfillableCapacityErrorCodes, *err.ErrorCode)
}

func isLaunchTemplateNotFound(err error) bool {
	if err == nil {
		return false
	}
	var awsError awserr.Error
	if errors.As(err, &awsError) {
		return awsError.Code() == launchTemplateNotFoundCode
	}
	return false
}
