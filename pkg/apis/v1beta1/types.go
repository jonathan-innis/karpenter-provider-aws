package v1beta1

import (
	"github.com/aws/aws-sdk-go/service/ec2"
)

type FiltersAndOwners struct {
	Filters []*ec2.Filter
	Owners  []string
}
